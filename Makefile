.PHONY: all build linux windows run tidy vet clean

all: build

# Build the server for the current host.
build:
	go build -o bin/samvnc-server ./cmd/samvnc-server

# Cross-compile for Linux (X11) and Windows (GDI).
linux:
	GOOS=linux GOARCH=amd64 go build -o bin/samvnc-server-linux-amd64 ./cmd/samvnc-server

windows:
	GOOS=windows GOARCH=amd64 go build -o bin/samvnc-server-windows-amd64.exe ./cmd/samvnc-server

run: build
	./bin/samvnc-server -verbose

tidy:
	go mod tidy

vet:
	go vet ./...

clean:
	rm -rf bin dist
