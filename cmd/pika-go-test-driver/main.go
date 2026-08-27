package main

import (
	"context"
	"os"

	"github.com/reyoung/pika-go/internal/testdriver"
)

func main() {
	os.Exit(testdriver.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
