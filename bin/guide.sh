#!/bin/bash -ex

cd "$(dirname "$0")"/..

go build -o bin/guide ./cmd/guide
bin/guide
