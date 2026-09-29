module github.com/lehigh-university-libraries/hawkeye

go 1.24.4

// HTR request-option patches; see third_party/htr/README.md.
replace github.com/lehigh-university-libraries/htr => ./third_party/htr

require (
	github.com/lehigh-university-libraries/htr v0.17.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)
