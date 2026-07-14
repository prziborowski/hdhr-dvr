#!/bin/bash -ex

cd "$(dirname "$0")"/..

go build -o bin/app cmd/app/app.go
go build -o bin/guide cmd/guide/guide.go
go build -o bin/auto-record cmd/auto-record/main.go
