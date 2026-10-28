//go:build darwin && arm64

package clientbin

import _ "embed"

const name = "aitun-client-darwin-arm64"

//go:embed aitun-client-darwin-arm64
var embedded []byte
