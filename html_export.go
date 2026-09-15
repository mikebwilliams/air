package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const htmlExportMaxDiffLines = 80

type htmlExportReport struct {
	Version       int                 `json:"version"`
	Title         string              `json:"title"`
	Repository    string              `json:"repository"`
	GeneratedAt   string              `json:"generated_at"`
	Findings      []htmlExportFinding `json:"findings"`
	Snapshot      bool                `json:"snapshot,omitempty"`
	ScanID        string              `json:"scan_id,omitempty"`
	InitialStatus string              `json:"initial_status,omitempty"`
	Verification  string              `json:"verification,omitempty"`
}

type htmlExportFinding struct {
	ID            int64               `json:"id"`
	Severity      string              `json:"severity"`
	Disposition   string              `json:"disposition"`
	Title         string              `json:"title"`
	Description   string              `json:"description"`
	File          string              `json:"file,omitempty"`
	Line          *int                `json:"line,omitempty"`
	Symbol        string              `json:"symbol,omitempty"`
	IntroducedSHA string              `json:"introduced_sha,omitempty"`
	ObservedSHA   string              `json:"observed_sha,omitempty"`
	ObservedAt    *time.Time          `json:"observed_at,omitempty"`
	ScanID        string              `json:"scan_id,omitempty"`
	TaskID        string              `json:"task_id,omitempty"`
	AttemptID     int64               `json:"attempt_id,omitempty"`
	Verifications []auditVerification `json:"verifications,omitempty"`
	Author        string              `json:"author,omitempty"`
	CommitDate    string              `json:"commit_date,omitempty"`
	ResolvedSHA   string              `json:"resolved_sha,omitempty"`
	DismissedAt   string              `json:"dismissed_at,omitempty"`
	DismissReason string              `json:"dismiss_reason,omitempty"`
	Tags          []string            `json:"tags,omitempty"`
	Review        *htmlExportReview   `json:"review,omitempty"`
	Events        []htmlExportEvent   `json:"events"`
	Diff          htmlExportDiff      `json:"diff"`
}

type htmlExportReview struct {
	Number          int    `json:"number"`
	Harness         string `json:"harness"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ReviewedAt      string `json:"reviewed_at"`
}

type htmlExportEvent struct {
	Action    string `json:"action"`
	Note      string `json:"note,omitempty"`
	SHA       string `json:"sha,omitempty"`
	CreatedAt string `json:"created_at"`
}

type htmlExportDiff struct {
	File          string   `json:"file,omitempty"`
	CommitSHA     string   `json:"commit_sha,omitempty"`
	HunkHeader    string   `json:"hunk_header,omitempty"`
	Lines         []string `json:"lines"`
	Target        int      `json:"target"`
	Message       string   `json:"message,omitempty"`
	Error         string   `json:"error,omitempty"`
	OmittedBefore int      `json:"omitted_before,omitempty"`
	OmittedAfter  int      `json:"omitted_after,omitempty"`
}

func writeHTMLExport(
	ctx context.Context,
	output io.Writer,
	repository *GitRepository,
	store *Store,
	now time.Time,
) error {
	report, err := buildHTMLExport(ctx, repository, store, now)
	if err != nil {
		return err
	}
	return writeHTMLExportReport(output, report)
}

func writeHTMLExportReport(output io.Writer, report htmlExportReport) error {
	prefix := htmlExportPrefix
	if report.Title != "" {
		prefix = strings.ReplaceAll(prefix, "AIR Findings Report", html.EscapeString(report.Title))
	}
	if _, err := io.WriteString(output, prefix); err != nil {
		return fmt.Errorf("write HTML export: %w", err)
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write HTML export data: %w", err)
	}
	if _, err := io.WriteString(output, htmlExportSuffix); err != nil {
		return fmt.Errorf("write HTML export: %w", err)
	}
	return nil
}

func buildHTMLExport(
	ctx context.Context,
	repository *GitRepository,
	store *Store,
	now time.Time,
) (htmlExportReport, error) {
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return htmlExportReport{}, err
	}
	display, err := loadFindingDisplayMetadata(ctx, repository, findings)
	if err != nil {
		return htmlExportReport{}, err
	}
	report := htmlExportReport{
		Version:     1,
		Title:       "AIR Findings Report",
		Repository:  filepath.Base(repository.WorkTree),
		GeneratedAt: now.Format(time.RFC3339),
		Findings:    make([]htmlExportFinding, 0, len(findings)),
	}
	for _, finding := range findings {
		events, err := store.FindingEvents(ctx, finding.ID)
		if err != nil {
			return htmlExportReport{}, err
		}
		review, err := store.FindingReview(ctx, finding.ID)
		if err != nil {
			return htmlExportReport{}, err
		}
		preview, previewErr := loadFindingDiffPreview(ctx, repository, finding)
		report.Findings = append(report.Findings,
			makeHTMLExportFinding(finding, display[finding.ID], review, events, preview, previewErr))
	}
	return report, nil
}

func makeHTMLExportFinding(
	finding Finding,
	display findingDisplayMetadata,
	review FindingReview,
	events []FindingEvent,
	preview findingDiffPreview,
	previewErr error,
) htmlExportFinding {
	exported := htmlExportFinding{
		ID: finding.ID, Severity: finding.Severity, Disposition: findingDisposition(finding),
		Title: finding.Title, Description: finding.Description, IntroducedSHA: finding.IntroducedSHA,
		ObservedSHA: finding.ObservedSHA, ObservedAt: finding.ObservedAt, ScanID: finding.ScanID,
		TaskID: finding.TaskID, AttemptID: finding.AttemptID, Verifications: finding.Verifications,
		Author: display.Blame, DismissReason: finding.DismissReason, Tags: finding.Tags, Line: finding.Line,
		Review: makeHTMLExportReview(review), Events: makeHTMLExportEvents(events),
		Diff: makeHTMLExportDiff(preview, previewErr),
	}
	if !display.CommitDate.IsZero() {
		exported.CommitDate = display.CommitDate.Format(time.RFC3339)
	}
	if finding.File != nil {
		exported.File = *finding.File
	}
	if finding.Symbol != nil {
		exported.Symbol = *finding.Symbol
	}
	if finding.ResolvedSHA != nil {
		exported.ResolvedSHA = *finding.ResolvedSHA
	}
	if finding.DismissedAt != nil {
		exported.DismissedAt = finding.DismissedAt.Format(time.RFC3339)
	}
	return exported
}

func makeHTMLExportReview(review FindingReview) *htmlExportReview {
	if review.ID == 0 {
		return nil
	}
	return &htmlExportReview{
		Number: review.Number, Harness: review.Harness,
		Model: review.Model, ReasoningEffort: review.ReasoningEffort,
		ReviewedAt: review.ReviewedAt.Format(time.RFC3339),
	}
}

func makeHTMLExportEvents(events []FindingEvent) []htmlExportEvent {
	exported := make([]htmlExportEvent, 0, len(events))
	for _, event := range events {
		exportedEvent := htmlExportEvent{
			Action: event.Action, Note: event.Note, CreatedAt: event.CreatedAt.Format(time.RFC3339),
		}
		if event.SHA != nil {
			exportedEvent.SHA = *event.SHA
		}
		exported = append(exported, exportedEvent)
	}
	return exported
}

func makeHTMLExportDiff(preview findingDiffPreview, previewErr error) htmlExportDiff {
	exported := htmlExportDiff{
		File: preview.File, CommitSHA: preview.CommitSHA, HunkHeader: preview.HunkHeader,
		Lines: make([]string, 0), Target: preview.Target, Message: preview.Message,
	}
	if previewErr != nil {
		exported.Error = previewErr.Error()
		return exported
	}
	if len(preview.Lines) <= htmlExportMaxDiffLines {
		exported.Lines = append(exported.Lines, preview.Lines...)
		return exported
	}
	start := preview.Target - htmlExportMaxDiffLines/2
	if start < 0 {
		start = 0
	}
	if maximum := len(preview.Lines) - htmlExportMaxDiffLines; start > maximum {
		start = maximum
	}
	end := start + htmlExportMaxDiffLines
	exported.Lines = append(exported.Lines, preview.Lines[start:end]...)
	exported.Target = preview.Target - start
	exported.OmittedBefore = start
	exported.OmittedAfter = len(preview.Lines) - end
	return exported
}

const htmlExportPrefix = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:">
<title>AIR Findings Report</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--panel:#fff;--raised:#f0f2f5;--text:#18202b;--muted:#667085;--border:#d8dde5;--accent:#1769e0;--accent-soft:#e7f0ff;--error:#b42318;--error-bg:#fee4e2;--warning:#9a6700;--warning-bg:#fff1c2;--info:#026aa2;--info-bg:#dff4ff;--open:#067647;--dismissed:#7f56d9;--resolved:#087ea4;--add:#067647;--del:#b42318;--code:#f8fafc;--target:#fff0a8;--shadow:0 8px 24px rgba(16,24,40,.08)}
@media(prefers-color-scheme:dark){:root{--bg:#0d1117;--panel:#161b22;--raised:#20262e;--text:#e6edf3;--muted:#9da7b3;--border:#30363d;--accent:#58a6ff;--accent-soft:#172b4d;--error:#ff7b72;--error-bg:#3d1c1c;--warning:#e3b341;--warning-bg:#372d12;--info:#79c0ff;--info-bg:#122b3c;--open:#56d364;--dismissed:#d2a8ff;--resolved:#76e3ea;--add:#56d364;--del:#ff7b72;--code:#0d1117;--target:#4b4218;--shadow:none}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}button,input,select{font:inherit;color:inherit}button{cursor:pointer}.page{max-width:1500px;margin:auto;padding:24px}.masthead{display:flex;gap:24px;justify-content:space-between;align-items:flex-start;margin-bottom:18px}.masthead h1{font-size:24px;margin:0 0 3px}.muted{color:var(--muted)}.summary{display:flex;flex-wrap:wrap;gap:8px;justify-content:flex-end}.count{padding:5px 10px;border:1px solid var(--border);border-radius:999px;background:var(--panel);white-space:nowrap}.toolbar{display:grid;grid-template-columns:minmax(220px,1fr) repeat(3,minmax(125px,auto));gap:10px;margin-bottom:12px}.toolbar.tags-enabled,.toolbar.verification-enabled{grid-template-columns:minmax(220px,1fr) repeat(4,minmax(125px,auto))}.toolbar.tags-enabled.verification-enabled{grid-template-columns:minmax(220px,1fr) repeat(5,minmax(125px,auto))}.toolbar input,.toolbar select,.tag-filter>summary{width:100%;border:1px solid var(--border);border-radius:8px;background:var(--panel);padding:9px 11px}.tag-filter{position:relative;min-width:145px}.tag-filter>summary{cursor:pointer;display:flex;align-items:center;justify-content:space-between;gap:10px;list-style:none;white-space:nowrap}.tag-filter>summary::-webkit-details-marker{display:none}.tag-filter>summary::after{content:'▾';color:var(--muted)}.tag-filter[open]>summary::after{content:'▴'}.tag-options{position:absolute;z-index:5;top:calc(100% + 5px);right:0;min-width:max(230px,100%);max-height:320px;overflow:auto;padding:7px;background:var(--panel);border:1px solid var(--border);border-radius:8px;box-shadow:var(--shadow)}.tag-option{display:flex;align-items:center;gap:8px;padding:6px 7px;border-radius:6px;cursor:pointer;white-space:nowrap}.tag-option:hover{background:var(--raised)}.tag-option input{width:auto;margin:0}.tag-option-count{color:var(--muted);margin-left:auto;font-variant-numeric:tabular-nums}.tag-clear{width:100%;margin-top:5px;padding:7px;border:1px solid var(--border);border-radius:6px;background:var(--raised)}.workspace{display:grid;grid-template-columns:minmax(330px,38%) minmax(0,62%);height:calc(100vh - 185px);min-height:520px;border:1px solid var(--border);border-radius:12px;background:var(--panel);box-shadow:var(--shadow);overflow:hidden}.finding-list{overflow:auto;border-right:1px solid var(--border);background:var(--raised)}.list-status{padding:9px 12px;color:var(--muted);border-bottom:1px solid var(--border);position:sticky;top:0;background:var(--raised);z-index:1}.finding-row{display:block;width:100%;padding:11px 12px;text-align:left;border:0;border-bottom:1px solid var(--border);background:transparent}.finding-row:hover{background:var(--panel)}.finding-row.selected{background:var(--accent-soft);box-shadow:inset 3px 0 var(--accent)}.row-top,.badges{display:flex;align-items:center;gap:7px;flex-wrap:wrap}.finding-id{font-variant-numeric:tabular-nums;color:var(--muted)}.badge{display:inline-block;padding:2px 7px;border-radius:999px;font-size:11px;font-weight:700;text-transform:uppercase;letter-spacing:.03em}.severity-error{color:var(--error);background:var(--error-bg)}.severity-warning{color:var(--warning);background:var(--warning-bg)}.severity-info{color:var(--info);background:var(--info-bg)}.disposition{background:var(--panel);border:1px solid currentColor}.disposition-open{color:var(--open)}.disposition-dismissed{color:var(--dismissed)}.disposition-resolved{color:var(--resolved)}.row-title{font-weight:650;margin:6px 0 3px}.row-location{color:var(--muted);font:12px ui-monospace,SFMono-Regular,Consolas,monospace;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.detail{overflow:auto;padding:22px 24px;scrollbar-gutter:stable}.empty{display:grid;place-items:center;height:100%;color:var(--muted)}.detail h2{font-size:22px;line-height:1.25;margin:0 0 10px}.detail h3{font-size:14px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted);margin:24px 0 9px}.description{font-size:15px;white-space:pre-wrap}.metadata{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:10px;margin:18px 0}.meta{padding:10px 12px;background:var(--raised);border-radius:8px;min-width:0}.meta dt{font-size:11px;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);margin-bottom:3px}.meta dd{margin:0;overflow-wrap:anywhere}.mono{font-family:ui-monospace,SFMono-Regular,Consolas,monospace}.diff{background:var(--code);border:1px solid var(--border);border-radius:8px;overflow:auto;padding:9px 0;font:12px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace}.diff-line{white-space:pre;min-width:max-content;padding:0 12px}.diff-add{color:var(--add)}.diff-del{color:var(--del)}.diff-hunk{color:var(--info);font-weight:700}.diff-target{background:var(--target);font-weight:700}.diff-omit{color:var(--muted);font-style:italic}.timeline{list-style:none;padding:0;margin:0}.timeline li{border-left:2px solid var(--border);padding:0 0 14px 14px;margin-left:5px}.event-title{font-weight:650}.keyboard{margin-top:8px;font-size:12px;color:var(--muted)}kbd{font:11px ui-monospace,monospace;border:1px solid var(--border);border-bottom-width:2px;border-radius:4px;padding:1px 4px;background:var(--raised)}
@media(max-width:800px){.page{padding:14px}.masthead{display:block}.summary{justify-content:flex-start;margin-top:12px}.toolbar,.toolbar.tags-enabled,.toolbar.verification-enabled,.toolbar.tags-enabled.verification-enabled{grid-template-columns:1fr 1fr}.toolbar .search{grid-column:1/-1}.tag-options{left:0;right:auto}.workspace{display:block;height:auto;min-height:0}.finding-list{height:42vh;border-right:0;border-bottom:1px solid var(--border)}.detail{min-height:55vh;padding:18px}}
@media print{body{background:#fff}.page{max-width:none;padding:0}.toolbar,.finding-list,.keyboard{display:none}.workspace{display:block;height:auto;border:0;box-shadow:none}.detail{overflow:visible;padding:0}.masthead{border-bottom:1px solid #bbb;padding-bottom:12px}}
</style>
</head>
<body>
<main class="page">
<header class="masthead"><div><h1 id="report-title">AIR Findings Report</h1><div id="report-meta" class="muted"></div></div><div id="summary" class="summary"></div></header>
<section class="toolbar" aria-label="Finding controls">
<input id="search" class="search" type="search" aria-label="Text search" placeholder="Search titles, descriptions, source, and notes (/)">
<select id="status" aria-label="Status"><option value="open">Open</option><option value="all">All statuses</option><option value="dismissed">Dismissed</option><option value="resolved">Resolved</option></select>
<select id="severity" aria-label="Severity"><option value="all">All severities</option><option value="error">Errors</option><option value="warning">Warnings</option><option value="info">Info</option></select>
<select id="verification" aria-label="Verification" hidden><option value="all">All verdicts</option><option value="unchecked">Unchecked</option><option value="confirmed">Confirmed</option><option value="false_positive">False positive</option><option value="uncertain">Uncertain</option></select>
<details id="tag-filter" class="tag-filter" hidden><summary id="tag-summary">All tags</summary><div id="tag-options" class="tag-options" role="group" aria-label="Required tags"></div></details>
<select id="sort" aria-label="Sort"><option value="id">ID (newest first)</option><option value="age">Age (oldest first)</option><option value="file">File and line</option><option value="author">Author</option><option value="severity">Severity</option><option value="status">Status</option><option value="title">Title</option></select>
</section>
<section class="workspace"><aside class="finding-list"><div id="list-status" class="list-status"></div><div id="finding-rows"></div></aside><article id="detail" class="detail"></article></section>
<div class="keyboard"><kbd>↑</kbd>/<kbd>↓</kbd> or <kbd>j</kbd>/<kbd>k</kbd> select &nbsp; <kbd>←</kbd>/<kbd>→</kbd> sort &nbsp; <kbd>/</kbd> search</div>
</main>
<script id="air-data" type="application/json">`

const htmlExportSuffix = `</script>
<script>
(function(){
'use strict';
var report=JSON.parse(document.getElementById('air-data').textContent);
var state={query:'',status:report.initial_status||'open',severity:'all',verification:report.verification||'all',tags:[],sort:'id',selected:null};
var rows=document.getElementById('finding-rows');
var detail=document.getElementById('detail');
var search=document.getElementById('search');
var statusSelect=document.getElementById('status');
var severitySelect=document.getElementById('severity');
var sortSelect=document.getElementById('sort');
var verificationSelect=document.getElementById('verification');
var toolbar=document.querySelector('.toolbar');
var tagFilter=document.getElementById('tag-filter');
var tagSummary=document.getElementById('tag-summary');
var tagOptions=document.getElementById('tag-options');
var searchIndex=new WeakMap();
function node(tag,className,text){var n=document.createElement(tag);if(className)n.className=className;if(text!==undefined)n.textContent=text;return n}
function shortSHA(value){return value?value.slice(0,12):''}
function location(f){if(!f.file)return 'No file location';return f.file+(f.line?':'+f.line:'')+(f.symbol?'  '+f.symbol:'')}
function addBadge(parent,text,className){parent.appendChild(node('span','badge '+className,text))}
function findingDate(f){return f.observed_at||f.commit_date}
function lastVerification(f){var checks=f.verifications||[];return checks.length?checks[checks.length-1]:null}
function verdict(f){var check=lastVerification(f);return check?check.outcome:'unchecked'}
function verdictLabel(value){return value.replace(/_/g,' ')}
function age(f){if(!findingDate(f))return 'Unknown age';var duration=Math.max(0,new Date(report.generated_at)-new Date(findingDate(f))),minute=60000,hour=60*minute,day=24*hour;if(duration<minute)return '<1m';if(duration<hour)return Math.floor(duration/minute)+'m';if(duration<day)return Math.floor(duration/hour)+'h';if(duration<30*day)return Math.floor(duration/day)+'d';if(duration<365*day)return Math.floor(duration/(30*day))+'mo';return Math.floor(duration/(365*day))+'y'}
function findingTags(f){return f.tags||[]}
function searchable(f){var cached=searchIndex.get(f);if(cached!==undefined)return cached;var d=f.diff||{};var review=f.review?[f.review.number,f.review.harness,f.review.model,f.review.reasoning_effort,f.review.reviewed_at].join(' '):'';var events=(f.events||[]).map(function(event){return [event.action,event.note,event.sha,event.created_at].join(' ')}).join(' ');var checks=(f.verifications||[]).map(function(check){return [check.outcome,check.reason,check.harness,check.model,check.effort,check.scan_id,check.attempt_id,check.checked_at].join(' ')}).join(' ');var text=[f.id,f.severity,f.disposition,f.title,f.description,f.file,f.line,f.symbol,findingTags(f).join(' '),f.author,f.commit_date,f.introduced_sha,f.resolved_sha,f.dismiss_reason,f.observed_sha,f.observed_at,f.scan_id,f.task_id,f.attempt_id,verdictLabel(verdict(f)),review,events,checks,d.file,d.commit_sha,d.hunk_header,(d.lines||[]).join('\n'),d.message,d.error].join(' ').toLowerCase();searchIndex.set(f,text);return text}
function matchesSearch(f){var terms=state.query.trim().toLowerCase().split(/\s+/).filter(Boolean);if(!terms.length)return true;var text=searchable(f);return terms.every(function(term){return text.indexOf(term)!==-1})}
function matchesTags(f){var tags=findingTags(f);return state.tags.every(function(tag){return tags.indexOf(tag)!==-1})}
function tagCounts(){var counts={};report.findings.forEach(function(f){findingTags(f).forEach(function(tag){counts[tag]=(counts[tag]||0)+1})});return counts}
function renderTagSummary(){tagSummary.textContent=state.tags.length?state.tags.length===1?'Tag: '+state.tags[0]:state.tags.length+' tags (AND)':'All tags'}
function renderTagOptions(){var counts=tagCounts(),tags=Object.keys(counts).sort();tagFilter.hidden=!tags.length;toolbar.classList.toggle('tags-enabled',tags.length>0);tagOptions.replaceChildren();var clear=node('button','tag-clear','Clear tag filters');clear.type='button';tags.forEach(function(tag){var label=node('label','tag-option');var checkbox=node('input');checkbox.type='checkbox';checkbox.checked=state.tags.indexOf(tag)!==-1;checkbox.addEventListener('change',function(){if(checkbox.checked)state.tags.push(tag);else state.tags=state.tags.filter(function(selected){return selected!==tag});state.tags.sort();clear.disabled=!state.tags.length;renderTagSummary();render()});label.append(checkbox,node('span','',tag),node('span','tag-option-count',counts[tag]));tagOptions.appendChild(label)});clear.disabled=!state.tags.length;clear.addEventListener('click',function(){state.tags=[];tagOptions.querySelectorAll('input').forEach(function(checkbox){checkbox.checked=false});clear.disabled=true;renderTagSummary();render()});tagOptions.appendChild(clear);renderTagSummary()}
function compareFile(a,b){var af=(a.file||'\uffff').toLowerCase(),bf=(b.file||'\uffff').toLowerCase();if(af!==bf)return af.localeCompare(bf);var al=a.line||Number.MAX_SAFE_INTEGER,bl=b.line||Number.MAX_SAFE_INTEGER;if(al!==bl)return al-bl;return b.id-a.id}
function compareText(a,b){var af=(a||'\uffff').toLowerCase(),bf=(b||'\uffff').toLowerCase();return af.localeCompare(bf)}
function visibleFindings(){var result=report.findings.filter(function(f){return(state.status==='all'||f.disposition===state.status)&&(state.severity==='all'||f.severity===state.severity)&&(state.verification==='all'||verdict(f)===state.verification)&&matchesTags(f)&&matchesSearch(f)});result.sort(function(a,b){var d=0;if(state.sort==='age')d=(findingDate(a)?Date.parse(findingDate(a)):Number.MAX_SAFE_INTEGER)-(findingDate(b)?Date.parse(findingDate(b)):Number.MAX_SAFE_INTEGER);else if(state.sort==='file')return compareFile(a,b);else if(state.sort==='author')d=compareText(a.author,b.author);else if(state.sort==='severity')d=({error:0,warning:1,info:2}[a.severity]||0)-({error:0,warning:1,info:2}[b.severity]||0);else if(state.sort==='status')d=({open:0,dismissed:1,resolved:2}[a.disposition]||0)-({open:0,dismissed:1,resolved:2}[b.disposition]||0);else if(state.sort==='title')d=compareText(a.title,b.title);return d||b.id-a.id});return result}
function renderSummary(){
  var counts={open:0,dismissed:0,resolved:0},verdicts={unchecked:0,confirmed:0,false_positive:0,uncertain:0};
  report.findings.forEach(function(f){counts[f.disposition]++;verdicts[verdict(f)]++});
  var summary=document.getElementById('summary');summary.replaceChildren();
  var items=[['Total',report.findings.length],['Open',counts.open],['Dismissed',counts.dismissed]];
  if(!report.snapshot)items.push(['Resolved',counts.resolved]);
  if(report.snapshot)Object.keys(verdicts).forEach(function(key){items.push([verdictLabel(key),verdicts[key]])});
  items.forEach(function(item){summary.appendChild(node('span','count',item[0]+': '+item[1]))});
}
function renderList(items){rows.replaceChildren();document.getElementById('list-status').textContent=items.length+' of '+report.findings.length+' findings';items.forEach(function(f){var row=node('button','finding-row'+(f.id===state.selected?' selected':''));row.type='button';row.setAttribute('aria-label','Finding '+f.id+': '+f.title);var top=node('div','row-top');top.appendChild(node('span','finding-id','#'+f.id));addBadge(top,f.severity,'severity-'+f.severity);addBadge(top,f.disposition,'disposition disposition-'+f.disposition);if(report.snapshot)addBadge(top,verdictLabel(verdict(f)),'verification-'+verdict(f));top.appendChild(node('span','muted',age(f)));if(f.author)top.appendChild(node('span','muted',f.author));row.appendChild(top);row.appendChild(node('div','row-title',f.title));row.appendChild(node('div','row-location',location(f)));row.addEventListener('click',function(){state.selected=f.id;render();if(window.innerWidth<=800)detail.scrollIntoView({behavior:'smooth',block:'start'})});rows.appendChild(row)})}
function addMeta(container,label,value,mono){if(!value)return;var box=node('div','meta');var dt=node('dt','',label);var dd=node('dd',mono?'mono':'',value);box.append(dt,dd);container.appendChild(box)}
function renderDiff(f){var section=node('section');section.appendChild(node('h3','',report.snapshot?'Source at observed snapshot':'Introducing diff'));var d=f.diff||{};if(d.error){section.appendChild(node('p','muted','Unavailable: '+d.error));return section}if(d.message){section.appendChild(node('p','muted',d.message));return section}if(!d.lines||!d.lines.length){section.appendChild(node('p','muted',report.snapshot?'No source excerpt is available.':'No diff excerpt is available.'));return section}var box=node('div','diff');if(d.hunk_header)box.appendChild(node('div','diff-line diff-hunk','  '+d.hunk_header));if(d.omitted_before)box.appendChild(node('div','diff-line diff-omit','  … '+d.omitted_before+' earlier lines omitted …'));d.lines.forEach(function(line,index){var cls='diff-line';if(line.charAt(0)==='+')cls+=' diff-add';else if(line.charAt(0)==='-')cls+=' diff-del';if(index===d.target)cls+=' diff-target';box.appendChild(node('div',cls,(index===d.target?'› ':'  ')+line))});if(d.omitted_after)box.appendChild(node('div','diff-line diff-omit','  … '+d.omitted_after+' later lines omitted …'));section.appendChild(box);return section}
function renderHistory(f){var section=node('section');section.appendChild(node('h3','','History'));if(!f.events.length){section.appendChild(node('p','muted','No recorded history.'));return section}var list=node('ol','timeline');f.events.forEach(function(event){var item=node('li');item.appendChild(node('div','event-title',event.action+(event.sha?'  '+shortSHA(event.sha):'')));item.appendChild(node('div','muted',new Date(event.created_at).toLocaleString()));if(event.note)item.appendChild(node('div','',event.note));list.appendChild(item)});section.appendChild(list);return section}
function renderVerifications(f){
  var section=node('section');section.appendChild(node('h3','','Rechecks'));
  var checks=f.verifications||[];
  if(!checks.length){section.appendChild(node('p','muted','Unchecked: no completed verification.'));return section}
  var list=node('ol','timeline');
  checks.slice().reverse().forEach(function(check,index){
    var item=node('li');var title=node('div','event-title');
    addBadge(title,verdictLabel(check.outcome),'verification-'+check.outcome);
    if(index===0)title.appendChild(node('span','muted',' Latest'));
    item.appendChild(title);
    item.appendChild(node('div','muted',check.harness+'/'+check.model+'/'+check.effort+' · '+new Date(check.checked_at).toLocaleString()));
    item.appendChild(node('div','description',check.reason));
    item.appendChild(node('div','muted mono','Recheck '+check.scan_id+' · attempt '+check.attempt_id));
    list.appendChild(item);
  });
  section.appendChild(list);return section;
}
function addTagBadge(parent,tag){var badge=node('button','badge disposition tag-badge',tag);badge.type='button';badge.title='Require tag '+tag;badge.addEventListener('click',function(){if(state.tags.indexOf(tag)===-1){state.tags.push(tag);state.tags.sort();renderTagOptions();render()}});parent.appendChild(badge)}
function renderDetail(f){detail.replaceChildren();if(!f){detail.appendChild(node('div','empty','No findings match the current filters.'));return}detail.appendChild(node('h2','',f.title));var badges=node('div','badges');addBadge(badges,'#'+f.id,'disposition disposition-'+f.disposition);addBadge(badges,f.severity,'severity-'+f.severity);addBadge(badges,f.disposition,'disposition disposition-'+f.disposition);findingTags(f).forEach(function(tag){addTagBadge(badges,tag)});detail.appendChild(badges);detail.appendChild(node('h3','','Description'));detail.appendChild(node('div','description',f.description));var metadata=node('dl','metadata');addMeta(metadata,'Location',location(f),true);addMeta(metadata,'Tags',findingTags(f).join(', '),true);if(report.snapshot){addMeta(metadata,'Observed snapshot',f.observed_sha,true);addMeta(metadata,'Observed',f.observed_at?new Date(f.observed_at).toLocaleString():'');addMeta(metadata,'Observation age',age(f));addMeta(metadata,'Scan',f.scan_id,true);addMeta(metadata,'Assignment',f.task_id,true);addMeta(metadata,'Attempt',f.attempt_id)}else{addMeta(metadata,'Introduced',shortSHA(f.introduced_sha),true);addMeta(metadata,'Commit age',age(f));addMeta(metadata,'Author',f.author);addMeta(metadata,'Commit date',f.commit_date?new Date(f.commit_date).toLocaleString():'')}addMeta(metadata,'Resolved',shortSHA(f.resolved_sha),true);addMeta(metadata,'Dismissed',f.dismissed_at?new Date(f.dismissed_at).toLocaleString():'');addMeta(metadata,'Dismissal reason',f.dismiss_reason);if(f.review){addMeta(metadata,'Review','#'+f.review.number+'  '+(f.review.harness&&f.review.harness!='codex'?f.review.harness+':':'')+f.review.model+(f.review.reasoning_effort?'/'+f.review.reasoning_effort:''));addMeta(metadata,'Reviewed',new Date(f.review.reviewed_at).toLocaleString())}detail.appendChild(metadata);if(report.snapshot)detail.appendChild(renderVerifications(f));detail.appendChild(renderDiff(f));detail.appendChild(renderHistory(f))}
function render(){var items=visibleFindings();if(!items.some(function(f){return f.id===state.selected}))state.selected=items.length?items[0].id:null;renderList(items);renderDetail(items.find(function(f){return f.id===state.selected})||null)}
search.addEventListener('input',function(){state.query=search.value;render()});search.addEventListener('keydown',function(event){if(event.key==='Escape'){search.value='';state.query='';search.blur();render()}});statusSelect.addEventListener('change',function(){state.status=statusSelect.value;render()});severitySelect.addEventListener('change',function(){state.severity=severitySelect.value;render()});sortSelect.addEventListener('change',function(){state.sort=sortSelect.value;render()});verificationSelect.addEventListener('change',function(){state.verification=verificationSelect.value;render()});
document.addEventListener('keydown',function(event){var form=/^(INPUT|SELECT|TEXTAREA)$/.test(event.target.tagName);if(event.key==='/'&&!form){event.preventDefault();search.focus();return}if(form)return;var items=visibleFindings();var index=items.findIndex(function(f){return f.id===state.selected});if(event.key==='ArrowDown'||event.key==='j'){event.preventDefault();if(items.length)state.selected=items[Math.min(items.length-1,index+1)].id;render()}else if(event.key==='ArrowUp'||event.key==='k'){event.preventDefault();if(items.length)state.selected=items[Math.max(0,index<0?0:index-1)].id;render()}else if(event.key==='ArrowLeft'||event.key==='ArrowRight'){event.preventDefault();var modes=Array.from(sortSelect.options,function(option){return option.value});var position=modes.indexOf(state.sort)+(event.key==='ArrowRight'?1:-1);position=(position+modes.length)%modes.length;state.sort=modes[position];sortSelect.value=state.sort;render()}});
if(report.snapshot){verificationSelect.hidden=false;verificationSelect.value=state.verification;toolbar.classList.add('verification-enabled');sortSelect.querySelector('[value=author]').remove();statusSelect.querySelector('[value=resolved]').remove()}
statusSelect.value=state.status;
document.addEventListener('click',function(event){if(tagFilter.open&&!tagFilter.contains(event.target))tagFilter.open=false});
document.title=report.title||'Findings Report';document.getElementById('report-title').textContent=report.title||'Findings Report';document.getElementById('report-meta').textContent=report.repository+(report.scan_id?' · scan '+shortSHA(report.scan_id):'')+' · generated '+new Date(report.generated_at).toLocaleString();renderSummary();renderTagOptions();render();
})();
</script>
</body>
</html>
`
