#!/bin/bash -ex

cd "$(dirname "$0")"/..

go build -o bin/app cmd/app/app.go
bin/app
