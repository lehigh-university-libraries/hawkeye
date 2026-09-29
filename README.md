# Hawkeye

Flag PDFs and images that need sensitive-information review, using on-prem OCR
and a separate visual assessment model. Hawkeye identifies candidates; it does not redact
documents or certify them safe for publication.

## Install

After the first release is published, install from the same tap as HTR:

```sh
brew tap lehigh-university-libraries/homebrew https://github.com/lehigh-university-libraries/homebrew
brew install lehigh-university-libraries/homebrew/hawkeye
```

Upgrade with `brew update && brew upgrade hawkeye`. Release archives are also
published for Linux, macOS, and Windows (amd64 and arm64) on the
[releases page](https://github.com/lehigh-university-libraries/hawkeye/releases).

## Build and run

Requires Go 1.24.4+ to build, Docker with a running Linux-container daemon, and
access to your on-prem Ollama service. ImageMagick and Ghostscript run inside
Houdini; neither is required on the host.

```sh
go build -o hawkeye .
./hawkeye '/mnt/DLShare/scans/SC-MS-0357-Rodale/Series 2 - Robert Rodale' \
  -o rodale-report.jsonl
```

With no path, Hawkeye scans the current directory. Directories are recursive by
default; `-d 0` scans files directly inside the selected directory, `-d 1` also
includes its immediate subdirectories, and any nonnegative depth is supported.
A file path scans just that file. Symlinks are skipped (and rejected as the input
path). Supported extensions, case-insensitively: PDF, TIFF/TIF, JP2/JPX/J2K,
JPEG/JPG, PNG, GIF, WebP, BMP. Every PDF page and image frame is scanned.

```sh
./hawkeye ./scans -d 0 -o top-level.jsonl
./hawkeye ./check.tiff --include-text -o check.jsonl
./hawkeye ./scans --ollama-url http://localhost:11434 --model glm-ocr:bf16
./hawkeye --help
```

The default model is `glm-ocr:bf16`, matching the cloned HTR evaluation. The
endpoint defaults to `https://ollama.cc.lehigh.edu`; `OLLAMA_URL` or
`--ollama-url` overrides it. SET must install the selected model first. All
inference requests use `github.com/lehigh-university-libraries/htr/pkg/ollama`
v0.17.0 with a small [local patch](third_party/htr/README.md) to forward `num_ctx`, `format`, and `think`.
The patched source is included in this repository; no separate HTR checkout is needed.

Each inference requests a **16,384-token context window** by default. Override
it with `--num-ctx 32768`, or use `--num-ctx 0` to omit the request option and
keep the server/model default. Negative values are rejected. The server's
`OLLAMA_CONTEXT_LENGTH` sets a default, not a maximum; see the
[Ollama API example](https://docs.ollama.com/faq#how-can-i-specify-the-context-window-size).
Larger windows require more server memory and must fit the selected model.
Image tokenization varies by model, so Hawkeye does not estimate context from
image dimensions. The window must accommodate the image, prompt, full
transcription, and assessment; 16,384 is a starting point, not a guarantee that
every page fits. Debug logs include the requested `num_ctx`.

Use `--model MODEL_NAME` to select the OCR model and `--analysis-model MODEL_NAME`
to select the assessment model. Both must be installed Ollama vision models.
OCR defaults to `glm-ocr:bf16`; assessment defaults to `qwen3.5:latest`.
Before sending any documents, Hawkeye checks `/api/show` for the `vision`
capability on the selected model. Text-only models such as `gpt-oss` cannot
perform image OCR or the current image-based assessment; a nonempty reply from
one is not a valid transcription. Unavailable models, missing capability
metadata, and text-only models stop the run before a report is created.
Reports retain the OCR text by default so it can be inspected or analyzed later.
Use `--include-text=false` if only findings should be retained.

Requests run sequentially, with a **two-second pause before every Ollama inference
request**, including the first OCR call and each assessment call. Use
`--request-delay 5s` for a longer pause or `--request-delay 0` to disable it.
The pause is shown in progress output, responds to Ctrl+C, and does not count
toward the request timeout. It also applies after a failed request before the
next inference attempt. Model capability checks and Houdini requests are not
delayed; completed page records are saved immediately.

## Progress and debugging

Progress on stderr shows the document filename, page number/total, and active
step: page counting, Docker rendering, Houdini preparation, or OCR, or assessment.
During a long step, a heartbeat prints every 10 seconds with elapsed time. Each
completed page shows its status, finding count, and duration. JSONL output on
stdout remains separate when using `-o -`.

```sh
export LOG_LEVEL=DEBUG
export OLLAMA_URL=http://your-ollama-api:11434
./hawkeye ./scans --model glm-ocr:bf16 \
  --houdini-url https://isle-microservices.cc.lehigh.edu/houdini \
  -o debug-report.jsonl 2>debug.log
```

Debug logs include configured endpoints, model names, conversion settings,
image sizes in bytes, request durations, token counts, and error details. They
exclude OCR text, image payloads, and upstream response bodies. Unset `LOG_LEVEL`
or set it to `INFO` to disable debug logs; normal progress remains visible.

OCR errors now distinguish HTTP failures, connection failures, invalid JSON,
and missing response fields. The HTR client calls `/api/generate`: `OLLAMA_URL`
must address the Ollama API, not an Open WebUI frontend. A useful check is
`curl "$OLLAMA_URL/api/tags"`, which should return model-list JSON rather than HTML.

## Separate OCR and model assessment

Each page uses two inference calls through HTR:

1. `--model` transcribes the prepared image using the documented GLM-OCR prompt,
   `Text Recognition:`, without a JSON schema or a request for PII judgments.
2. `--analysis-model` independently inspects the same image and the OCR text for
   bank accounts, routing numbers, SSNs, and credit cards. It receives an
   [assessment-only JSON schema](assessment_schema.json). It does not rewrite
   the saved transcription. Assessment sends `think=false` so the structured
   result is returned as the final answer rather than reasoning output.

This replaces the combined request, which produced unsupported PII claims with
GLM-OCR. A response schema constrains structure; it does not establish accuracy.
Both models are checked for image support before documents are processed.
The assessment sees the image as well as the text so that an OCR omission does
not automatically hide a visual finding. Document text is explicitly treated
as untrusted data, not instructions.

```sh
./hawkeye ./scans --model glm-ocr:bf16 --analysis-model qwen3.5:latest \
  --num-ctx 16384 -o report.jsonl
```

Hawkeye independently checks the OCR using account-number and bank/MICR context,
routing checksums, formatted SSNs, and Luhn-valid card-number rules. Findings
contain line numbers and categories, never matched values. Addresses and phone
numbers alone are not targets. `--no-regex` disables these rules, not either
model call. `--include-text=false` omits the stored transcript, but both rules
and assessment still receive it.

The report keeps `text` from the OCR model and `assessment` from the assessment
model; `ocr_model` and `analysis_model` identify each. OCR and rule findings
remain in the page record if assessment fails, times out, or is interrupted.
An OCR request failure skips assessment. Errors do not become `no_findings`.

The percentage is an **uncalibrated model estimate**, not a measured probability.
Any rule finding, model finding, empty OCR, or unreadable assessment results in
review. A model negative cannot erase a rule finding. Missing fields, invalid
JSON, or contradictory categories produce errors requiring review. Illegible
text alone is not evidence for any particular sensitive category.

Validate both models on representative pages with known findings and negatives
before scanning the whole collection. Synthetic checks establish basic behavior,
not accuracy on historical handwriting or a guarantee of finding all PII.

## Image preparation

Hawkeye uses a pinned multi-architecture `islandora/houdini` image; override it with
`--docker-image islandora/houdini:main` to follow the main tag. Docker can pull
the default image on first use. Containers
have no network, a read-only root filesystem, dropped capabilities, and bounded
memory, CPU, processes, and temporary storage. Input is streamed over stdin;
source directories are never mounted. Use a local or trusted on-prem Docker
daemon because it receives the documents. Conversion times out per operation;
Ctrl-C/SIGTERM also attempts to remove the active container.

Hawkeye invokes ImageMagick directly because the upstream Houdini `cmd.sh` selects
only the first PDF/TIFF page. Pages render at 200 DPI and are reduced to at most
2400 pixels on the longest edge, without upscaling; `--dpi` and `--max-edge`
are configurable. `--max-edge 0` retains the rendered dimensions. These defaults
are a starting point, **not a proven minimum legible size**: validate fine print
and MICR digits against representative scans before batching the collection.

To use the Houdini image optimizer:

```sh
OLLAMA_URL=https://ollama.cc.lehigh.edu \
  ./hawkeye ./scans --houdini-url http://your-on-prem-houdini:8080/ \
  -o optimized.jsonl
```

Single-frame images (including TIFF/JP2) are uploaded as their original bytes,
with the appropriate `Content-Type` and `Accept: image/png` to select the output
format (Houdini requires a single destination MIME type). PDFs and multiframe
images are split locally in Docker, then uploaded one page at a time as `image/png`. The endpoint
must return one optimized PNG or JPEG directly in a successful HTTP 200 response.
Redirects, non-images, and responses over 32 MiB are rejected. The service owns
the sizing policy for direct uploads; no undocumented `X-Islandora-Args`
contract is assumed. Docker is still required to enumerate frames. For maximum
input detail to the optimizer, use `--max-edge 0` and tune PDF DPI as needed.

## Reports and review

Each JSONL record has an absolute `file`, a **one-based** `page`, model names,
`status`, and `findings`. `page: 0` means the document could not be enumerated;
the entire document needs review. Status values:

- `review`: rules or model flagged a candidate, OCR was empty, or the model
  reported unreadable text.
- `no_findings`: processing completed without a flag; this is not a safety claim.
- `error`: enumeration, rendering, OCR, or assessment failed; review or retry.
  Preparation errors include the cause (such as Houdini's HTTP status or Docker's
  exit status), without recording HTTP response bodies or conversion stderr.

Reports are created with mode `0600` and appended record by record. Passing an
existing output file resumes that report. Each record is synced to disk before
progress marks the page complete; the JSONL file itself is the checkpoint.
This does not depend on shutdown handlers running. A failed write or sync stops
the scan immediately. `-o -` writes JSONL to stdout; progress
and a final count go to stderr. The `text` field retains OCR text by default,
including when assessment validation fails, for inspection and later analysis.
Use `--include-text=false` to omit transcripts. Reports can contain sensitive
text and filenames; protect them and endpoint logs accordingly.

```sh
# Every document with a flagged page OR a processing error:
jq -r 'select(.status != "no_findings") | .file' rodale-report.jsonl | sort -u

# Specific pages to inspect:
jq -r 'select(.status != "no_findings") | [.file, .page, .status] | @tsv' \
  rodale-report.jsonl
```

Exit code 0 means every discovered page was processed (findings are allowed).
Exit code 1 means invalid input, interrupted execution, failed output, or one
or more processing errors. Page failures do not stop the remaining batch.
On Ctrl+C/SIGTERM, completed page records remain in the report, and a page whose
active request was canceled is recorded as an error. No full-document completion
is needed to save earlier pages. Generated images are held in memory or the
container's temporary filesystem, not written into the input directory. Hawkeye
cancels requests and attempts to remove the active Docker container before
exiting; removal depends on Docker being reachable. Remote Houdini manages its
own temporary files.

An OOM kill or SIGKILL cannot run cleanup handlers. Previously synced page
records remain available; the current page may be lost, and a kill during a
write can leave an incomplete last line. On resume, Hawkeye validates complete
records and discards only an unfinished final JSON object before appending.
Malformed complete records stop the run without changing the report. A valid
final record missing its newline is preserved and separated from new records. With `-o -`, flushing and durability depend on the receiving
process. An active Docker container may survive a host-process kill until its
command finishes; `--rm` removes it when it exits. Disk or filesystem failures
can still prevent successful checkpoints.

For an interrupted run, rerun the same command with the same output report:

```sh
./hawkeye /path/to/images -o /path/to/images/report.jsonl
# After a crash or interruption, run that same command again.
```

To process a bounded batch, keep the same report and request limit on each run:

```sh
./hawkeye /path/to/images --limit 500 -o /path/to/images/report.jsonl
```

`--limit` caps Ollama inference attempts in this invocation, including failed
attempts. OCR and assessment each count as one, so 500 normally completes 250
pages. Capability checks, Docker/Houdini operations, and skipped pages do not
count. Hawkeye starts a page only when at least two requests remain, saves its
result, and stops before exceeding the limit. An odd limit may leave one request
unused. The default `0` is unlimited; positive limits must be at least `2`.
Reaching the limit is a normal exit; processing errors still cause a nonzero
exit. Rerun the command to skip completed pages and retry errors or scan the
remaining pages. A report written to stdout (`-o -`) cannot be resumed.

Resume keys are absolute file paths and one-based page numbers. The latest
`review` or `no_findings` record completes a page; `error` records are retried.
A review finding is a completed scan, not a processing failure. New files are
scanned, and successful pages in partially scanned PDFs/TIFFs are skipped.
New records include `page_count`, allowing fully completed multipage files to
be skipped without conversion. Older single-image reports also work; older
PDF/TIFF/GIF/WebP reports need page enumeration before completed pages can be
skipped. If all files are complete, Docker and Ollama are not contacted.

Historical records remain in the report. Retries append a new result; consumers
should use the latest record for each `(file, page)`. A successful page record
supersedes an older page-0 enumeration error. The web viewer applies these rules.
The CLI summary describes work performed in this invocation and lists skips.
`-o -` does not resume. Use only one scanner per report at a time.

Resume assumes source files and scan settings have not changed. It does not
invalidate results when a file, prompt, model, or detection rule changes.
**Use a new report to reevaluate earlier results**, including false positives
from older detection rules. Start with known positive and negative pages and
manually measure missed detections before trusting the review workload.

The implementation is sequential, has no automatic retries within a run,
and streams a source document into a fresh container for each conversion. This
bounds working state but adds overhead for large PDFs. Add persistent conversion
workers only if the first batch demonstrates that need.

## Read-only report viewer

The same binary includes a web application; serving reports needs neither
Docker nor Ollama. Put each collection's report under its directory:

```text
/srv/reports/
  Rodale Series 2/report.jsonl
  Other collection/report.json
```

```sh
hawkeye serve /srv/reports --listen 127.0.0.1:8080
```

For colleagues, run it on a shared server behind your institutional login/TLS
reverse proxy. Use `--listen :8080` if the proxy connects across a container
network. The application has no built-in login. Run it as a user with read
access to the report directories, or mount that directory read-only in the
server environment. Scanning can run separately and append reports while the
viewer is open; report files are never changed by the web application.

The index discovers files ending in `jsonl` and `.json` files with `report` in
their name, recursively, and links them by directory. Contents must be Hawkeye
JSONL, even when the extension is `.json`. Report symlinks are ignored.
Each report shows completed files, scanned pages, review candidates, and
processing errors, using the latest attempt for each page. Counts reflect
saved records, not an overall percentage: the report does not list inputs
that have not yet produced a record. A timestamp shows the last saved update;
an old timestamp cannot establish whether a scanner is still running.

The default table shows pages needing attention. Switch to **All saved pages**
to include negative results. **Copy table for Sheets** copies an HTML table and
tab-separated text; ordinary browser selection/copy also works. Auto-refresh
runs every ten seconds, pauses during text selection, and can be disabled.
Copying the table disables refresh until reenabled. OCR transcripts and source
documents are not served. The application accepts only read requests.

## Development checks

```sh
make build
make lint               # golangci-lint v2.13.2
make test
make test-integration   # requires Docker
make release-check      # GoReleaser v2.18.2, inside a Git checkout
govulncheck ./...
gosec -quiet ./...
```

The Docker integration check uses synthetic two-page PDF and TIFF documents and
a local mock Ollama server. It checks distinct page rendering, HTR API requests,
failure reporting, report permissions, and suppression of sensitive error bodies.
It does not measure model accuracy or validate the live Lehigh services.
An opt-in [live model check](testdata/README.md) sends two synthetic printed pages
through the actual OCR and assessment clients. It is excluded from normal CI.

## CI and releases

CI follows HTR's GitHub Actions setup. Pushes and pull requests check formatting,
run golangci-lint, run unit and Docker integration tests with the race detector,
and build a GoReleaser snapshot without publishing it. The Make targets above run the same
checks locally. Go is selected from `go.mod`.

As in HTR, merged PRs to `main` call Lehigh's shared `bump-release.yaml` workflow,
which creates a `v`-prefixed tag and dispatches `goreleaser.yaml` on that tag.
Use `skip-release` in the PR title to skip automatic releases. Tag pushes also
trigger GoReleaser; manual dispatch must select a `v`-prefixed tag. Release jobs
run tests before uploading archives, checksums, and the `hawkeye` Homebrew
formula to `lehigh-university-libraries/homebrew`.

Repository setup: make the existing `HOMEBREW_REPO_GHAT` secret available to
Hawkeye, with write access to Hawkeye releases and the Homebrew tap. Enable
GitHub Actions and access to Lehigh's shared workflow. The ordinary workflow
token needs contents/actions write permissions for automatic tagging and
dispatch. No secret is used in PR test jobs. These files configure publication;
they do not create the GitHub repository, provision secrets, or publish a release.

GoReleaser is pinned to v2.18.2 because the existing tap uses its deprecated but
still supported `brews` publisher for Linux and macOS formulas, matching HTR.
Snapshot builds validate packaging and formula generation without publishing.
Review formula compatibility before upgrading GoReleaser.

References: [Houdini wrapper](https://github.com/Islandora-Devops/isle-buildkit/blob/main/images/houdini/rootfs/app/cmd.sh),
[GLM-OCR usage](https://ollama.com/library/glm-ocr),
[Ollama structured outputs](https://docs.ollama.com/capabilities/structured-outputs).
