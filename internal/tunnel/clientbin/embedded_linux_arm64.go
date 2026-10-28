//go:build linux && arm64

package clientbin

import _ "embed"

const name = "aitun-client-linux-arm64"

//go:embed aitun-client-linux-arm64
var embedded []byte
