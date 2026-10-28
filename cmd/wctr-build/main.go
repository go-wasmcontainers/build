// Command wctr-build builds an image from a Containerfile with a wctrd
// engine: `wctr-build -H unix:///path -t name .`. It is the build command
// on its own, until wctr serves it as `wctr build`.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/build/cli"
)

var osExit = os.Exit

// RootCmd is the program's command.
func RootCmd() *cobra.Command {
	cmd := cli.Command()
	cmd.Use = "wctr-build [OPTIONS] PATH | -"
	cmd.PersistentFlags().StringP("host", "H", "", "engine socket (unix:///path; default $WCTR_HOST, else "+cli.DefaultHost+")")
	return cmd
}

func main() {
	cmd := RootCmd()
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
		osExit(1)
	}
}
