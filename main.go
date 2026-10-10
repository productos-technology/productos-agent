// productos-agent: the ProductOS Agent executable. See README.md.
package main

import (
	"os"

	"github.com/Hamdan85/productos-agent/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
