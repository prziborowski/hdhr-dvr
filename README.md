# HDHomeRun DVR

A simple web-based DVR application for HDHomeRun tuners that allows you to schedule and view TV recordings.

## Features

- Schedule TV recordings from available channels
- View and manage scheduled recordings
- Download completed recordings
- HTTP Range support for streaming recordings to VLC and other media players
- SQLite database for storing recording schedules
- Configurable storage directory via `config.json`

## Installation

### Prerequisites

- HDHomeRun tuner connected to your network
- Go 1.16+ installed
- ffmpeg installed and in PATH

### Building

```bash
git clone https://github.com/prziborowski/hdhr-dvr.git
cd hdhr-dvr
bin/build.sh
```

This builds three binaries: `bin/app`, `bin/guide`, `bin/auto-record`.

### Running

Start the DVR app:

```bash
bin/app    # Starts web UI on http://localhost:8080
```

Fetch EPG guide data from TitanTV:

```bash
bin/guide   # Fetches EPG from TitanTV and pushes it to the app (POST /api/guide)
```

Auto-schedule recordings by keyword:

```bash
bin/auto-record   # Matches keywords against guide and schedules recordings
```

## Configuration

Copy `example.json` to `config.json` as a starting point:

```bash
cp example.json config.json
```

Edit the fields in `config.json`:

| Field | Required | Description |
|-------|----------|-------------|
| `timezone` | No | Go timezone (e.g., `America/Los_Angeles`). Defaults to `America/Los_Angeles`. |
| `lineUpID` | Yes | Your TitanTV lineup ID. Obtain from your TitanTV account. |
| `userId` | Yes | Your TitanTV user ID. Obtain from your TitanTV account. |
| `days` | Yes | Number of EPG days to fetch (max 8). |
| `storageDir` | Yes | Directory where recorded files are saved. |
To obtain `lineUpID` and `userId`:

1. Create a TitanTV account at [titantv.com](https://www.titantv.com)
2. Set up your lineup to scan for local channels
3. Inspect browser cookies/API requests from the TitanTV web interface to extract these values

### Database

The application uses SQLite at `./recordings.db`. The database is created automatically on first run. The EPG guide is also stored in the database (new `guide_channels`/`guide_programs`/`guide_meta` tables) instead of a shared `guide.json` file.

### Usage

1. Access the web interface at http://localhost:8080
1. Select a channel from the dropdown
1. Choose a date using the calendar
1. Set the start time and duration
1. Click "Schedule Recording"
1. View scheduled recordings in the list below
1. Download completed recordings using the provided links

## API Endpoints

### Channels

* `GET /api/channels` - List available channels
* `POST /api/recordings` - Create a new recording
```json
{
   "channelId": "12345",
   "date": "2026-01-01",
   "startTime": "19:00",
   "duration": 60
}
```
* `DELETE /api/recordings/{id}` - Delete a recording
* `GET /api/recordings/{id}/file` - Download a recording file

### Guide

* `POST /api/guide` - Replace the app's in-memory and stored guide with a full EPG snapshot (channels + programs + generated timestamp). This is how `bin/guide` updates the guide; the app persists it to SQLite so the guide survives restarts with no shared `guide.json` required. `PUT /api/guide` is accepted as an alias.
```json
{
    "channels": [ ... ],
    "programs": [ ... ],
    "generated": "2026-01-01T00:00:00Z"
}
```

## Development

### Building

All three binaries are built together:

```bash
bin/build.sh          # Produces bin/app, bin/guide, bin/auto-record
```

Individual compilation:

```bash
go build -o bin/app cmd/app/app.go
go build -o bin/guide cmd/guide/guide.go
go build -o bin/auto-record cmd/auto-record/main.go
```

### Running in Development

```bash
bin/app    # Starts web UI on http://localhost:8080
```

## License

MIT

