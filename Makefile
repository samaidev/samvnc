.PHONY: all build tray linux windows run tidy vet clean

all: build

# Build the server for the current host.
build:
	go build -o bin/samvnc-server ./cmd/samvnc-server

# Build the tray app (system tray icon that manages samvnc-server).
tray:
	go build -o bin/samvnc-tray ./cmd/samvnc-tray

# Build both server and tray.
all-build: build tray

# Cross-compile for Linux (X11) and Windows (GDI).
linux:
	GOOS=linux GOARCH=amd64 go build -o bin/samvnc-server-linux-amd64 ./cmd/samvnc-server
	GOOS=linux GOARCH=amd64 go build -o bin/samvnc-tray-linux-amd64 ./cmd/samvnc-tray

windows:
	GOOS=windows GOARCH=amd64 go build -o bin/samvnc-server-windows-amd64.exe ./cmd/samvnc-server
	GOOS=windows GOARCH=amd64 go build -o bin/samvnc-tray-windows-amd64.exe ./cmd/samvnc-tray

macos:
	GOOS=darwin GOARCH=amd64 go build -o bin/samvnc-server-darwin-amd64 ./cmd/samvnc-server
	GOOS=darwin GOARCH=amd64 go build -o bin/samvnc-tray-darwin-amd64 ./cmd/samvnc-tray

run: build
	./bin/samvnc-server -verbose

run-tray: tray
	./bin/samvnc-tray

tidy:
	go mod tidy

vet:
	go vet ./...

clean:
	rm -rf bin dist
