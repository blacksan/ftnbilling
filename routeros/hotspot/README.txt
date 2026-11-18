hotspot-ispbill: custom hotspot pages for ispbill.

Upload this WHOLE folder to the router root (so it shows up as "hotspot-ispbill"
in /file/print), using any of:
  - WinBox: Files > drag the folder in (or create hotspot-ispbill and drop the files)
  - scp -r hotspot admin@ROUTER_IP:/hotspot-ispbill   (SFTP/SSH must be enabled)
  - FTP: create the folder, then upload each file

The hotspot profile must point at it:
  /ip/hotspot/profile set ispbill-hsprof html-directory=hotspot-ispbill

If the hotspot complains about a missing page (e.g. md5.js, redirect.html), copy
the router's default "hotspot" folder to "hotspot-ispbill" first, then overwrite
it with these files. After editing, hotspot pages are re-read without a restart.
