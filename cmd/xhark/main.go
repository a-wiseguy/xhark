package main

import (
	"fmt"
	"os"

	"xhark/internal/ui"
)

func main() {
	app := ui.NewApp(os.Stdin, os.Stdout)
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
