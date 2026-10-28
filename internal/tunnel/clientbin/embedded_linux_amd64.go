//go:build linux && amd64

package clientbin

import _ "embed"

const name = "aitun-client-linux-amd64"

//go:embed aitun-client-linux-amd64
var embedded []byte
