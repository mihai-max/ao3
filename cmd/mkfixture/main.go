// Command mkfixture writes a small sample data folder of synthetic
// AO3-style EPUBs for trying the app without the real torrent: two zips, or
// with -rar a single things.rar holding them the way Real-Debrid serves
// multi-file torrents.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mihai-max/ao3/internal/testutil"
)

func main() {
	rar := flag.Bool("rar", false, "write one things.rar containing the zips instead of loose zips")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mkfixture [-rar] DIR")
		os.Exit(2)
	}
	write := testutil.WriteSampleDataDir
	if *rar {
		write = testutil.WriteSampleRarDir
	}
	if err := write(flag.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("sample data written to", flag.Arg(0))
}
