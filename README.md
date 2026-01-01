# HDHomeRun DVR

A simple web-based DVR application for HDHomeRun tuners that allows you to schedule and view TV recordings.

## Features

- Schedule TV recordings from available channels
- View and manage scheduled recordings
- Download completed recordings
- HTTP Range support for streaming recordings to VLC and other media players
- SQLite database for storing recording schedules
- Configurable storage directory via environment variable

## Installation

### Prerequisites

- HDHomeRun tuner connected to your network
- Go 1.16+ installed
- ffmpeg installed and in PATH

### Building

```bash
git clone https://github.com/prziborowski/hdhr-dvr.git
cd hdhr-dvr
go build
```

### Running

# Basic usage (uses default storage directory)
./hdhr-dvr

# With custom storage directory
STORAGE_DIR=/your/custom/path ./hdhr-dvr

## Configuration

### Environment Variables

* STORAGE_DIR: Directory where recordings will be stored (default: /data/Storage/record)

### Database

The application uses SQLite for storing recording schedules. The database file (recordings.db) will be created automatically in the application directory.

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
```
{
  "channelId": "12345",
  "date": "2026-01-01",
  "startTime": "19:00",
  "duration": 60
}
```
* `DELETE /api/recordings/{id}` - Delete a recording
* `GET /api/recordings/{id}/file` - Download a recording file

## Development

### Running in Development
```
go run app.go
```

## License

MIT



