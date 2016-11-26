Creating a new base VM image
============================

From an old base image
----------------------

1. Copy the current base image to a new file: `cp base-gentoo-vanilla.img base-gentoo-vanilla-v2.img`.
2. Manually configure networking for a new VM which you will manually start:
  * Add a new VLAN on both the hypervisor and torcontrol.
  * Add a bridge for the new VM.
  * etc etc...
3. Start a new VM to use: `screen -S vm-rebuild-world qemu-system-x86_64 -nographic -enable-kvm -cpu host -curses -m 4096M -drive "file=/root/vm-images/base-gentoo-vanilla-v2.img,if=virtio" -netdev "tap,helper=/usr/libexec/qemu-bridge-helper --br=br100,id=hn0" -device "virtio-net-pci,netdev=hn0,id=nic1" -append "root=/dev/vda4 ro vmid=100" -kernel "/root/vm-images/kernels/vmlinuz-4.1.7-hardened-r1"`.
4. Peform the upgrade/update (e.g. apt-get update && apt-get dist-upgrade).
5. Clean the image for usage in a VM:
  * Remove the history: `unset HISTFILE && rm ~/.bash_history`.
  * Remove pre-generated SSH keys: `rm -rf /etc/ssh/ssh_host_*`.
  * Remove logs generated from running processes: `ls -lah /var/log` then delete relevant files.
6. Shut down VM: `shutdown -h now`.
7. Reconfigure hypervisor-daemon to use the new base image.
