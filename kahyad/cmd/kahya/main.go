// kahya is the Kâhya CLI. This is a build stub; W12-06 replaces it with the
// real CLI (one-shot, REPL, `kahya log --trace <id>` over UDS).
package main

import (
	"fmt"

	"kahya/kahyad/internal/buildinfo"
)

func main() {
	fmt.Printf("kahya %s (stub; replaced by W12-06)\n", buildinfo.Version)
}
