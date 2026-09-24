# Hawkeye

Flag PDFs and images that need sensitive-information review, using on-prem OCR
and optional visual assessment. Hawkeye identifies candidates; it does not redact
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
model requests use `github.com/lehigh-university-libraries/htr/pkg/ollama`
v0.17.0, the release at the reference checkout's commit. No local `replace`
directive or changes to the HTR checkout are needed.

Use `--model MODEL_NAME` to select another installed Ollama vision model for OCR.
`--analysis-model MODEL_NAME` selects the separate optional PII assessment model.
Model names must match the Ollama server's installed tags.
Before sending any documents, Hawkeye checks `/api/show` for the `vision`
capability on both selected models. Text-only models such as `gpt-oss` cannot
perform image OCR or the current image-based assessment; a nonempty reply from
one is not a valid transcription. Unavailable models, missing capability
metadata, and text-only models stop the run before a report is created.
Reports retain the OCR text by default so it can be inspected or analyzed later.
Use `--include-text=false` if only findings should be retained.

## Progress and debugging

Progress on stderr shows the document filename, page number/total, and active
step: page counting, Docker rendering, Houdini preparation, OCR, or assessment.
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

## OCR and optional model assessment

The default pipeline renders a page, asks GLM-OCR for text, and scans the text
with rules for account-number context, bank/MICR context, routing checksums,
formatted SSNs, and Luhn-valid card-number candidates. Findings include the
OCR line number, never the matched value. Addresses and telephone numbers by
themselves are not targets. Rules are heuristics and can produce false positives
and false negatives, especially with OCR substitutions, missing text, or unusual
number formatting. Both rules and model assessment can miss information.

GLM-OCR's documented text prompt is `Text Recognition:`. An OCR model should not
be assumed to support reliable instruction-following or PII probability estimates.
To add a visual PII assessment, select an installed vision instruction model:

```sh
./hawkeye ./scans --analysis-model YOUR_INSTALLED_VISION_MODEL -o assessed.jsonl
```

This makes a second call through HTR on each page image, asking for JSON with
`contains_sensitive`, `readable`, `sensitive_likelihood_percent` (0–100), and
`kinds`. The percentage is an **uncalibrated model estimate**, not a measured
probability, and is informational. Review status follows findings, the model's
sensitive-content flag, or its unreadable flag. Model assessments never erase
rule findings.

HTR v0.17.0 does not expose Ollama's `format` parameter. Hawkeye therefore requests
JSON in the prompt and validates required fields, types, ranges, and categories.
Invalid assessments become processing errors requiring review. `--no-regex`
allows model-only experiments and requires `--analysis-model`.

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

Reports are created with mode `0600`, appended record by record during the run,
and never overwrite an existing file. `-o -` writes JSONL to stdout; progress
and a final count go to stderr. The `text` field retains OCR text by default,
including when a subsequent assessment fails, for inspection and later analysis.
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

For an interrupted run, the partial report cannot establish coverage of the
collection; rerun to a new report. Start with known positive and negative pages
and manually measure missed detections before trusting the review workload.

The initial implementation is sequential, has no automatic retries or resume,
and streams a source document into a fresh container for each conversion. This
bounds working state but adds overhead for large PDFs. Add persistent conversion
workers or resume only if the first batch demonstrates that need.

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
