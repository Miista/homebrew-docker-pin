package main

import (
	"fmt"
	"time"
)

func main() {
	for {
		fmt.Println("alive: 2.0.0")
		time.Sleep(5 * time.Second)
	}
}
