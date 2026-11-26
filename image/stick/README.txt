USB GitHub Actions runner - settings folder
===========================================

Files you put in this "ghr" folder are read when the stick boots. All of
them are optional: you can set everything up from the web page instead.

  admin-password   The web page's admin password (one line, 10+ characters).
                   Used only if no password has been set yet, so the
                   first-visitor setup page never opens.

  runner.env       Registration settings. Copy runner.env.example to
                   runner.env and fill it in. It is applied again whenever
                   its contents change, so you can edit it on another
                   computer and reboot.

  *.pem, keys      Files that runner.env refers to (GitHub App private key,
                   SSH public keys).

After booting, the machine's screen shows the web page address
(https://ghr-XXXX.local/) and the certificate fingerprint to check.
