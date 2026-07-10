#!/bin/bash

printf '{"wasm":"%s","data":%s,"args":%s}' \
    "$(base64 -w0 app.wasm)" "${1:-[]}" "${2:-[]}" \
| curl -s -X POST http://localhost:3000/execute \
    -H "X-API-Key: $KEY" -H "Content-Type: application/json" --data-binary @-