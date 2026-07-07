// kahyad is the Kâhya control-plane daemon. W0-02 ships a buildable stub so
// W0-04 can codesign the binary; W12-01 replaces the internals.
package main

import (
	"fmt"

	"kahya/kahyad/internal/buildinfo"
)

func main() {
	fmt.Printf("kahyad %s (stub; replaced by W12-01)\n", buildinfo.Version)
}
