# HTR request-options patch

These two packages are copied from
[`github.com/lehigh-university-libraries/htr` v0.17.0](https://github.com/lehigh-university-libraries/htr/tree/v0.17.0),
the version previously used by Hawkeye. The Apache-2.0 license is included.
Only the packages needed by Hawkeye are included.

Changes from upstream:

- `providers.Config.NumCtx` carries the requested Ollama context window.
- `ollama.Provider.ExtractText` sends `options.num_ctx` when positive; zero
  leaves the server/model default in effect.
- `providers.Config.Format` forwards an optional JSON schema in the top-level
  `format` field, allowing Ollama to constrain the response structure.
- `providers.Config.Think` forwards the optional `think` boolean. Assessment
  uses `false` so Qwen returns its JSON as the final response, not reasoning.

The root `go.mod` replaces HTR with this checked-in module so ordinary builds
and releases include the patch. Hawkeye's request tests exercise it through
the real HTR client and a local mock server. No module-cache edits are needed.
Once HTR releases equivalent support, update Hawkeye to that API/version and
remove this replacement and directory.
