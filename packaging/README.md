# Package distribution

This directory holds the source-controlled parts of Halro package distribution.
The GitHub Release remains the immutable artifact source of truth.

- `debian/` is consumed directly by the main release workflow.
- `homebrew/` seeds the separate `halro-ai/homebrew-tap` repository.
- `apt-repository/` seeds the separate APT control-plane repository. Its public
  output is hosted at `https://packages.halro.ai/apt`.

Do not publish the Homebrew or APT channels from these seed directories. Move
them to their dedicated repositories, configure protected credentials and
review, run clean-host installation acceptance, and only then mark the channel
available on `halro.ai`.
