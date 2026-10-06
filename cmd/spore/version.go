package main

// version is the release this binary was built from. Release builds set it
// with -ldflags "-X main.version=<tag>"; anything else reports "dev".
var version = "dev"

// isVersionArg reports whether args ask for the version. It is answered before
// the config is loaded, so it works on a machine with no config file.
func isVersionArg(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "version", "--version", "-version", "-v":
		return true
	}
	return false
}
