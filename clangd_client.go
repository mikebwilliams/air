package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// A single clangd process serves the bounded indexing workers. The reader must
// keep draining notifications while requests are outstanding, including when a
// worker is canceled. Neither compilation commands nor shell strings execute.
type clangdClient struct {
	cmd         *exec.Cmd
	input       io.WriteCloser
	writeMu     sync.Mutex
	mu          sync.Mutex
	nextID      int
	pending     map[int]chan clangdReply
	diagnostics map[string]chan []semanticDiagnostic
	err         error
	done        chan struct{}
	log         boundedClangdLog
}

type clangdReply struct {
	result json.RawMessage
	err    error
}
type boundedClangdLog struct {
	mu   sync.Mutex
	data []byte
}

func (log *boundedClangdLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.data = append(log.data, data...)
	if len(log.data) > 16384 {
		log.data = append([]byte{}, log.data[len(log.data)-16384:]...)
	}
	return len(data), nil
}
func (log *boundedClangdLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return string(log.data)
}

func startClangd(ctx context.Context, executable, database, worktree string, jobs int) (*clangdClient, error) {
	client := &clangdClient{pending: map[int]chan clangdReply{}, diagnostics: map[string]chan []semanticDiagnostic{}, done: make(chan struct{})}
	args := append(append([]string{}, semanticClangdFlags...), "--compile-commands-dir="+database, fmt.Sprintf("-j=%d", jobs))
	client.cmd = exec.CommandContext(ctx, executable, args...)
	client.cmd.Dir = worktree
	client.cmd.Stderr = &client.log
	var err error
	client.input, err = client.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := client.cmd.StdoutPipe()
	if err != nil {
		client.input.Close()
		return nil, err
	}
	if err := client.cmd.Start(); err != nil {
		client.input.Close()
		output.Close()
		return nil, err
	}
	go client.read(output)
	var initialized struct {
		Capabilities struct {
			PositionEncoding       string          `json:"positionEncoding"`
			DocumentSymbolProvider bool            `json:"documentSymbolProvider"`
			DocumentLinkProvider   json.RawMessage `json:"documentLinkProvider"`
		} `json:"capabilities"`
	}
	err = client.request(ctx, "initialize", map[string]any{
		"processId": nil, "rootUri": clangdURI(worktree),
		"capabilities": map[string]any{"general": map[string]any{"positionEncodings": []string{"utf-8"}},
			"textDocument": map[string]any{"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true}}},
	}, &initialized)
	if err == nil && (initialized.Capabilities.PositionEncoding != "utf-8" || !initialized.Capabilities.DocumentSymbolProvider || len(initialized.Capabilities.DocumentLinkProvider) == 0) {
		err = errors.New("clangd must support UTF-8 positions, document symbols, and document links (use clangd 21+)")
	}
	if err == nil {
		err = client.notify("initialized", map[string]any{})
	}
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize clangd: %w", err)
	}
	return client, nil
}

func clangdURI(filename string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(filename)}).String()
}
func clangdPath(uri string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("unsupported clangd URI %q", uri)
	}
	return filepath.Clean(filepath.FromSlash(parsed.Path)), nil
}

func (client *clangdClient) send(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	_, err = fmt.Fprintf(client.input, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}
func (client *clangdClient) notify(method string, params any) error {
	return client.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (client *clangdClient) request(ctx context.Context, method string, params, output any) error {
	client.mu.Lock()
	if client.err != nil {
		err := client.err
		client.mu.Unlock()
		return err
	}
	client.nextID++
	id := client.nextID
	replies := make(chan clangdReply, 1)
	client.pending[id] = replies
	client.mu.Unlock()
	defer func() { client.mu.Lock(); delete(client.pending, id); client.mu.Unlock() }()
	if err := client.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		_ = client.notify("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	case reply := <-replies:
		if reply.err != nil {
			return reply.err
		}
		if output == nil {
			return nil
		}
		if err := json.Unmarshal(reply.result, output); err != nil {
			return fmt.Errorf("decode clangd %s: %w", method, err)
		}
		return nil
	}
}

func readClangdMessage(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for total := 0; ; {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		total += len(line)
		if total > 8192 {
			return nil, errors.New("oversized clangd message header")
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(key, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
		}
	}
	if length < 0 || length > 64<<20 {
		return nil, fmt.Errorf("invalid clangd Content-Length %d", length)
	}
	data := make([]byte, length)
	_, err := io.ReadFull(reader, data)
	return data, err
}

func (client *clangdClient) read(output io.Reader) {
	defer close(client.done)
	reader := bufio.NewReader(output)
	for {
		data, err := readClangdMessage(reader)
		if err != nil {
			client.fail(fmt.Errorf("clangd connection ended: %w", err))
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			client.fail(err)
			return
		}
		if message.Method != "" {
			if len(message.ID) > 0 { // We advertise no workspace operations; reject unexpected server requests.
				_ = client.send(map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "unsupported client method"}})
			} else if message.Method == "textDocument/publishDiagnostics" {
				var notification struct {
					URI         string               `json:"uri"`
					Version     int                  `json:"version"`
					Diagnostics []semanticDiagnostic `json:"diagnostics"`
				}
				if json.Unmarshal(message.Params, &notification) == nil && notification.Version == 1 {
					// clangd may escape URI characters differently (notably +).
					// Match canonical file paths rather than raw URI spellings.
					if filename, err := clangdPath(notification.URI); err == nil {
						notification.URI = clangdURI(filename)
					}
					client.mu.Lock()
					channel := client.diagnostics[notification.URI]
					client.mu.Unlock()
					if channel != nil {
						select {
						case channel <- notification.Diagnostics:
						default:
						}
					}
				}
			}
			continue
		}
		var id int
		if json.Unmarshal(message.ID, &id) != nil {
			continue
		}
		reply := clangdReply{result: message.Result}
		if message.Error != nil {
			reply.err = fmt.Errorf("clangd error %d: %s", message.Error.Code, message.Error.Message)
		}
		client.mu.Lock()
		channel := client.pending[id]
		client.mu.Unlock()
		if channel != nil {
			select {
			case channel <- reply:
			default:
			}
		}
	}
}
func (client *clangdClient) fail(err error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.err = err
	for _, channel := range client.pending {
		select {
		case channel <- clangdReply{err: err}:
		default:
		}
	}
}
func (client *clangdClient) Close() {
	// Killing a private analysis server also bounds shutdown after a stuck AST.
	_ = client.cmd.Process.Kill()
	_ = client.input.Close()
	<-client.done
	_ = client.cmd.Wait()
}

func (client *clangdClient) parse(ctx context.Context, filename, kind string, contents []byte, command InventoryCommand) ([]semanticSymbol, []semanticLink, []semanticDiagnostic, error) {
	uri := clangdURI(filename)
	diagnostics := make(chan []semanticDiagnostic, 1)
	client.mu.Lock()
	client.diagnostics[uri] = diagnostics
	client.mu.Unlock()
	defer func() {
		client.mu.Lock()
		delete(client.diagnostics, uri)
		client.mu.Unlock()
		_ = client.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
	}()
	// Supply the exact selected command, including a deterministic borrowed
	// command for headers, rather than relying on clangd's open-file history.
	if err := client.notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"compilationDatabaseChanges": map[string]any{filename: map[string]any{"workingDirectory": command.Directory, "compilationCommand": command.Arguments}}}}); err != nil {
		return nil, nil, nil, err
	}
	language := "cpp"
	if strings.EqualFold(filepath.Ext(filename), ".c") {
		language = "c"
	}
	if err := client.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": language, "version": 1, "text": string(contents)}}); err != nil {
		return nil, nil, nil, err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	symbols, links := []semanticSymbol{}, []semanticLink{}
	if err := client.request(ctx, "textDocument/documentSymbol", params, &symbols); err != nil {
		return nil, nil, nil, err
	}
	if err := client.request(ctx, "textDocument/documentLink", params, &links); err != nil {
		return nil, nil, nil, err
	}
	select {
	case diagnostic := <-diagnostics:
		return symbols, links, diagnostic, nil
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	case <-client.done:
		return nil, nil, nil, errors.New("clangd exited before publishing diagnostics")
	}
}
