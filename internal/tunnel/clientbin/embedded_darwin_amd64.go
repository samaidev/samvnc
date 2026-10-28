//go:build darwin && amd64

package clientbin

import _ "embed"

const name = "aitun-client-darwin-amd64"

//go:embed aitun-client-darwin-amd64
var embedded []byte
