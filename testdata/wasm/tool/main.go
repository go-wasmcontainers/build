// Command tool is a wasip1 test program for build steps:
//
//	tool write PATH TEXT     write TEXT to PATH (parents created)
//	tool envfile NAME PATH   write $NAME to PATH
//	tool cat PATH...         print files
//	tool pwd                 print the working directory
//	tool exit N              exit with status N
//
// Installed as /bin/sh it is also a builtin-only shell: `sh -c "tool-args"`
// splits its argument on spaces and runs it as above.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) == 2 && args[0] == "-c" {
		args = strings.Fields(args[1])
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "tool: no command")
		os.Exit(2)
	}
	switch args[0] {
	case "write", "echo-to":
		must(len(args) == 3)
		must(os.MkdirAll(filepath.Dir(args[1]), 0o755) == nil)
		if err := os.WriteFile(args[1], []byte(args[2]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "envfile":
		must(len(args) == 3)
		must(os.MkdirAll(filepath.Dir(args[2]), 0o755) == nil)
		must(os.WriteFile(args[2], []byte(os.Getenv(args[1])), 0o644) == nil)
	case "cat":
		for _, p := range args[1:] {
			b, err := os.ReadFile(p)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Stdout.Write(b)
		}
	case "pwd":
		d, _ := os.Getwd()
		fmt.Println(d)
	case "exit":
		n, _ := strconv.Atoi(args[1])
		fmt.Println("exiting", n)
		os.Exit(n)
	default:
		fmt.Fprintln(os.Stderr, "tool: unknown command", args[0])
		os.Exit(2)
	}
}

func must(ok bool) {
	if !ok {
		fmt.Fprintln(os.Stderr, "tool: bad usage")
		os.Exit(2)
	}
}
