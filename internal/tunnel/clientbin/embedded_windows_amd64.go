//go:build windows && amd64

package clientbin

import _ "embed"

// name is the file name the embedded client is extracted as. It matches
// the bundled-client naming convention used by internal/tunnel.
const name = "aitun-client-windows-amd64.exe"

//go:embed aitun-client-windows-amd64.exe
var embedded []byte
