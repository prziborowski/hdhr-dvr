# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27.1 AS build
ARG DMS_REF=latest
RUN CGO_ENABLED=0 go install -trimpath github.com/anacrolix/dms@${DMS_REF}

# ---- runtime ----
FROM alpine:3.21
RUN apk add --no-cache ffmpeg ffmpegthumbnailer mailcap

# Honor "$GOPATH/bin/dms": keep GOPATH=/go and put the binary there.
ENV GOPATH=/go
ENV PATH=$PATH:/go/bin

COPY --from=build /go/bin/dms /go/bin/dms

# Bake in the config (placeholder now, populated later / overridable via bind mount).
COPY dms.json /dms/dms.json
WORKDIR /dms

# Media source. Point the "Path" key in dms.json at this dir, then bind-mount it.
VOLUME /record

# 4338 = HTTP/DLNA control; 1900/udp = SSDP/UPnP discovery.
EXPOSE 4338 1900/udp

# Shell form so $GOPATH expands; exec so SIGTERM is delivered to dms.
ENTRYPOINT ["/bin/sh", "-c", "exec $GOPATH/bin/dms --config dms.json"]
