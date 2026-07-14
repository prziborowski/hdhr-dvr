#!/bin/bash -e

cd "$(dirname "$0")"/..

echo "=== Running all pkg tests ==="
go test -v ./pkg/...
echo "=== All tests passed ==="
