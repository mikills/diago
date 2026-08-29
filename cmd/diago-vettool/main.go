package main

import (
	"github.com/mikills/diago/diago"
	"golang.org/x/tools/go/analysis/unitchecker"
)

func main() {
	unitchecker.Main(diago.IteratorErrorAnalyzer)
}
