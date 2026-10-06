# appie-fill

`appie-fill` fills an Albert Heijn delivery order that you already planned, from the terminal. It lists your open orders, opens one for changes, searches the catalog, and sets quantities. It shows AH's own payable total. When you leave, it reverts the order at AH, so your account goes back to today's bonus prices.

All AH access goes through [appie-go](https://github.com/gwillem/appie-go).

## Install

You need Go 1.26 or later.

```sh
go install github.com/szwaan/appie/cmd/appie-fill@latest
```

## Run

```sh
appie-fill
```

To use a token file other than the default, pass `-config`:

```sh
appie-fill -config ~/ah/config.json
```

On Linux and macOS, only one `appie-fill` runs at a time. A second one exits with `appie-fill is already running`.

## Log in

appie-fill uses the browser login built into appie-go. On the first run, appie-go starts a local server, prints a login URL, and opens it in your browser. It uses `open` on macOS and `xdg-open` on Linux. On other systems, open the printed URL yourself and log in at AH.

appie-go saves the tokens in the appie config file, `$XDG_CONFIG_HOME/appie/config.json`. When `XDG_CONFIG_HOME` is not set, the file is `~/.config/appie/config.json`. The `appie` CLI reads the same file, so one login works for both. If AH rejects the stored login later, appie-fill runs the browser login again and returns to your orders.

## Fill an order

Select an order and press `enter`. appie-fill reopens it at AH. Every key acts at once. A `…` after a quantity means AH has not confirmed it yet, and the footer says `updating…` until AH has. The total on screen is AH's total, because group promotions such as "2e gratis" show up only there.

| Key | Action |
| --- | --- |
| `↑`, `↓` | Move between lines or search hits. |
| `+`, `-` | Add one, or take one away. |
| `0` to `9`, then `enter` | Type a quantity from 0 to 99. 0 removes the line. |
| `enter` | Type a new quantity for the line, or for the search hit. It starts from the quantity in the order, or 1. |
| `del` | Remove the line. |
| `u` | Undo the last removal. |
| `/` | Search the catalog. Products that AH cannot deliver are listed as `niet leverbaar` and cannot be added. |
| `r` | Retry a change that did not reach AH. |
| `esc` | Cancel a typed quantity, go back from search, or leave the order. |
| `q`, `ctrl+c` | Leave the order and quit. In search, `q` is a letter, so use `ctrl+c`. |

## Leave an order

While an order is reopened, AH gives your whole account the bonus prices of that delivery date. appie-fill leaves the order when you press `esc`, `q`, or `ctrl+c`, when you close the terminal, and when it gets a `kill`. It first sends the changes that AH has not confirmed, then reverts the order. If AH did not take a change, appie-fill names it.

After `kill -9` or a power cut, the order stays reopened at AH. The next start lists it with a `!`. Press `enter` to resume it, or `x` to leave it.

## What it does not do

appie-fill does not check out, pay, change the delivery slot, or show receipts. It works with AH in the Netherlands, not AH Belgium.

## License

GNU Affero General Public License v3.0, because appie-go uses that license. See [LICENSE](LICENSE).
