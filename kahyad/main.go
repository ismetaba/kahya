// kahyad is the Kâhya control-plane daemon. This is a build/codesign stub;
// W12-01 replaces it with the real daemon (config, UDS listener, JSONL logging).
package main

import (
	"fmt"

	"kahya/kahyad/internal/buildinfo"
)

func main() {
	fmt.Printf("kahyad %s (stub; replaced by W12-01)\n", buildinfo.Version)
}
