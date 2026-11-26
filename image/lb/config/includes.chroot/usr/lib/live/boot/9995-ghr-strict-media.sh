#!/bin/sh
# Sourced by live-boot's initramfs script after its own helpers, so this
# find_livefs replaces the stock one. The stock version falls back to
# scanning every disk (and mounting each filesystem read-only to look for
# /live) when the live-media= device isn't found. On a runner that would
# mean touching the host's disks, so only the medium named on the kernel
# command line (ghr's GHR-BOOT UUID) is ever tried; if it never shows up,
# live-boot gives up with "Unable to find a medium".

find_livefs ()
{
	if [ -n "${LIVE_MEDIA}" ] && check_dev "null" "${LIVE_MEDIA}" "skip_uuid_check"
	then
		return 0
	fi
	return 1
}
