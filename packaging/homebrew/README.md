# Official Halro Homebrew tap

This directory is the reviewed source for `halro-ai/homebrew-tap`. Keep the
Formula release URLs and SHA-256 values bound to the immutable Halro GitHub
Release, then run the tap's macOS and Linuxbrew CI before advertising
`brew install halro-ai/tap/halro`.

The main release owns binaries and checksums. The tap owns only Formula metadata
and must never rebuild or replace a published Halro release artifact.
