#!/bin/bash -ex

cd "$(dirname "$0")"/..

go build -o bin/app ./cmd/app/
go build -o bin/guide ./cmd/guide/
go build -o bin/auto-record ./cmd/auto-record/
