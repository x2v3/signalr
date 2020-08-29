[![GoDoc](https://godoc.org/github.com/x2v3/signalr?status.svg)](https://godoc.org/github.com/x2v3/signalr)
[![Build Status](https://travis-ci.org/x2v3/signalr.svg?branch=master)](https://travis-ci.org/x2v3/signalr)
[![Go Report Card](https://goreportcard.com/badge/github.com/x2v3/signalr)](https://goreportcard.com/report/github.com/x2v3/signalr)
[![Maintainability](https://api.codeclimate.com/v1/badges/c561e13d50cdd11e97a1/maintainability)](https://codeclimate.com/github/x2v3/signalr/maintainability)
[![codecov](https://codecov.io/gh/x2v3/signalr/branch/master/graph/badge.svg)](https://codecov.io/gh/x2v3/signalr)

# Project depricated

Unfortunately, I am no longer able to provide support for this project. Please see https://github.com/x2v3/signalr/network for some forks that have been created.

# Overview

This is my personal attempt at implementating the client side of the WebSocket
portion of the SignalR protocol. I use it for various virtual currency trading
platforms that use SignalR.

It supports CloudFlare-protected sites by default.

## Examples

Simple example:

```go
package main

import (
	"log"

	"github.com/x2v3/signalr"
)

func main() {
	// Prepare a SignalR client.
	c := signalr.New(
		"fake-server.definitely-not-real",
		"1.5",
		"/signalr",
		`[{"name":"awesomehub"}]`,
		nil,
	)

	// Define message and error handlers.
	msgHandler := func(msg signalr.Message) { log.Println(msg) }
	panicIfErr := func(err error) {
		if err != nil {
			log.Panic(err)
		}
	}

	// Start the connection.
	err := c.Run(msgHandler, panicIfErr)
	panicIfErr(err)

	// Wait indefinitely.
	select {}
}
```

Generic usage:

- [Basic usage](https://github.com/x2v3/signalr/blob/master/examples/basic/main.go)
- [Complex usage](https://github.com/x2v3/signalr/blob/master/examples/complex/main.go)

Cryptocurrency examples:

- [Bittrex](https://github.com/x2v3/signalr/blob/master/examples/bittrex/main.go)
- [Cryptopia](https://github.com/x2v3/signalr/blob/master/examples/cryptopia/main.go)

Proxy examples:

- [No authentication](https://github.com/x2v3/signalr/blob/master/examples/proxy-simple)
- [With authentication](https://github.com/x2v3/signalr/blob/master/examples/proxy-authenticated)

# Documentation

- GoDoc: https://godoc.org/github.com/x2v3/signalr
- SignalR specification: https://docs.microsoft.com/en-us/aspnet/signalr/overview/
- Excellent technical deep dive of the protocol: https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/

# Contribute

If anything is unclear or could be improved, please open an issue or submit a
pull request. Thanks!
