# Katnip

Katnip is a block explorer for the [Kaspa](https://github.com/kaspanet/kaspad/) project.

This is v.2 Katnip, based on [Katnip](https://github.com/someone235/katnip) by [someone235](https://github.com/someone235).

## Building

You need [Go](http://golang.org) (was tested on version 1.17).

```
git clone https://github.com/cbitensky/katnip.git
go build .
```

## Running

```
./katnip
```

By default it connects to Kaspad server at `0.0.0.0:16110`, creates database file `~/katnip.mdb` and runs HTTP server on port `80`. Than you can open http://0.0.0.0 by browser.

## CSS styling

HTTP server uses `style.css` file that have to be in program folder.