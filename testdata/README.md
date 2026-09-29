# Synthetic model checks

These PNGs are generated examples, not collection material:

- `ordinary-note.png`: “Please file this garden note.” No target PII.
- `bank-account.png`: “Bank account number: 12345678”. Invented number; expect
  only the bank-account category from the model.

Both are clearly printed and should be assessed as readable. They test basic
positive/negative behavior, not accuracy on handwriting or other real documents.

Run the opt-in test against your own Ollama endpoint:

```sh
OLLAMA_URL=https://ollama.cc.lehigh.edu:11434 HAWKEYE_OLLAMA_TEST=1 \
  go test -run '^TestLiveModels$' -v -timeout 15m .
```

Normal tests use local mock servers and do not send documents to Ollama.
