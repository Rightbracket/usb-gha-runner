.PHONY: all test image qemu-uefi qemu-bios qemu-recovery clean

all: test

test:
	cd ghr && go vet ./... && go test ./...

image:
	image/build.sh

qemu-uefi: out/ghr-usb.img
	test/qemu/run.sh out/ghr-usb.img uefi

qemu-bios: out/ghr-usb.img
	test/qemu/run.sh out/ghr-usb.img bios

qemu-recovery: out/ghr-usb.img
	test/qemu/recovery.sh out/ghr-usb.img uefi

clean:
	rm -rf out
