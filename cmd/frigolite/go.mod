module github.com/pijalu/frigolite/cmd/frigolite

go 1.23.0

require github.com/pijalu/frigolite v0.0.0

require (
	github.com/lmorg/murex v0.0.0-20250115225944-b4c429617fd4 // indirect
	github.com/lmorg/readline/v4 v4.2.4
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
)

replace github.com/pijalu/frigolite => ../../

replace github.com/lmorg/readline/v4 => ../../third_party/readline
