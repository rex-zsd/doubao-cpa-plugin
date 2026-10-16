# Third-party notices

The Doubao protocol, QR authentication, media upload and tool adapters in this repository were implemented independently in Go. No doubao-cli runtime or package is imported or executed. Its public source was consulted as protocol research: https://github.com/Fullstop000/doubao-cli.

The C ABI declarations and basic buffer/callback wiring in `cmd/cliproxy-plugin/abi.go` were adapted from CLIProxyAPI's MIT-licensed plugin examples.

The sole Go module dependency is gopkg.in/yaml.v3 v3.0.1 (MIT/Apache-2.0), used only to read plugin configuration. Its license is included in binary packages. Python integration tests use the official OpenAI SDK; it is not a runtime dependency of the plugin.
