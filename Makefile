.PHONY: all test image qemu-uefi qemu-bios clean

all: test

test:
	cd ghr && go vet ./... && go test ./...

image:
	image/build.sh

qemu-uefi: out/ghr-usb.img
	test/qemu/run.sh out/ghr-usb.img uefi

qemu-bios: out/ghr-usb.img
	test/qemu/run.sh out/ghr-usb.img bios

clean:
	rm -rf out
