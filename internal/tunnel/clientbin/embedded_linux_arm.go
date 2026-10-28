//go:build linux && arm

package clientbin

import _ "embed"

const name = "aitun-client-linux-arm"

//go:embed aitun-client-linux-arm
var embedded []byte
