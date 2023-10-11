
SOURCES := $(shell find . -name "*.go")

all: ipmond_linux_amd64 ipmond_linux_arm64 ipmond_linux_arm7

test: $(SOURCES)
	go test ./...

ipmond_linux_amd64: $(SOURCES)
	env GOOS=linux GOARCH=amd64 go build -o ipmond_linux_amd64 -ldflags '-s -w' bonan.se/ipmon/cmd/ipmond

ipmond_linux_arm64: $(SOURCES)
	env CC=arm-none-eabi-gcc CGO_ENABLED=0  GOOS=linux GOARCH=arm64 go build -buildmode=exe -o ipmond_linux_arm64 -ldflags '-extldflags "-fno-PIC static" -s -w' -tags 'osusergo netgo static_build' bonan.se/ipmon/cmd/ipmond

ipmond_linux_arm7: $(SOURCES)
	env CC=arm-none-eabi-gcc CGO_ENABLED=0  GOOS=linux GOARCH=arm GOARM=7 go build -buildmode=exe -o ipmond_linux_arm7 -ldflags '-extldflags "-fno-PIC static" -s -w' -tags 'osusergo netgo static_build' bonan.se/ipmon/cmd/ipmond

