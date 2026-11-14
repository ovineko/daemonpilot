# HTTP Server Example

Simple HTTP server managed by daemonpilot for testing purposes.

## Build

```bash
cd examples/http-server
go build -o http-server
```

## Run

```bash
./http-server run
```

## Test endpoints

```bash
# Health check
curl http://localhost:8080/health

# Ping
curl http://localhost:8080/ping

# Home
curl http://localhost:8080/
```

## Install as service

```bash
# Install as systemd service (Linux)
sudo ./http-server install

# Start the service
sudo ./http-server start

# Check status
sudo ./http-server status

# Stop the service
sudo ./http-server stop

# Uninstall
sudo ./http-server uninstall
```
