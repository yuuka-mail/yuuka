// Command yuuka is the entry point for the yuuka mail system.
package main

import "fmt"

// version is set at build time via -ldflags.
var version = "0.0.0-dev"

func main() {
	fmt.Printf("yuuka %s\n", version)
}
