// kahya is the CLI front-end to kahyad. W0-02 ships a buildable stub;
// W12-06 replaces the internals.
package main

import (
	"fmt"

	"kahya/kahyad/internal/buildinfo"
)

func main() {
	fmt.Printf("kahya %s (stub; replaced by W12-06)\n", buildinfo.Version)
}
