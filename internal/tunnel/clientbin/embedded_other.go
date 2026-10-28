//go:build !((windows && amd64) || (windows && arm64) || (linux && amd64) || (linux && arm64) || (linux && arm) || (darwin && amd64) || (darwin && arm64))

package clientbin

// Unsupported platform: no embedded client. Extract() reports a clear
// error and internal/tunnel falls back to PATH lookup / pip-installed
// clients as before.
const name = ""

var embedded []byte
