// Command mkfixture writes a small sample data folder (two zips of synthetic
// AO3-style EPUBs) for trying the app without the real torrent.
package main

import (
	"fmt"
	"os"

	"github.com/mihai-max/ao3/internal/testutil"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkfixture DIR")
		os.Exit(2)
	}
	if err := testutil.WriteSampleDataDir(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("sample data written to", os.Args[1])
}
