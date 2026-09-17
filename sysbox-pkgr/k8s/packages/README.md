# packages/ — Sysbox .deb staged for the sysbox-deploy-k8s image build

Place the **matching-arch** Sysbox CE `.deb` here before `docker build`.
The Dockerfile in the parent directory first looks for the release package
`<SYSBOX_DEB_PREFIX>-<arch>-<version>.deb`, where the prefix is the
`SYSBOX_DEB_PREFIX` build argument (default `sysbox-deb-linux`).

Also accepted (upstream packager naming):

```
sysbox-ce_<ver>.linux_amd64.deb
sysbox-ce_<ver>.linux_arm64.deb
```

Do not commit the `.deb` files — they are gitignored. Stage them from the
sysbox-pkgr build output.
