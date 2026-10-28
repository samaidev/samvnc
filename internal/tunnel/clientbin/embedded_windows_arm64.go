//go:build windows && arm64

package clientbin

import _ "embed"

const name = "aitun-client-windows-arm64.exe"

//go:embed aitun-client-windows-arm64.exe
var embedded []byte
