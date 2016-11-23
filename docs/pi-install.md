Torhost Install
===============

* This install requires two network interfaces to be present, but it is recommended to only connect the gateway/WAN interface until configuration is complete. This is only important if you have a working hypervisor with malicious users.
* The install is completed as two users: root and pi

*As root*
1. Install the Raspbian Jessie image from https://www.raspberrypi.org/downloads/raspbian/ to an SD card
2. Boot into Raspbian as normal
3. Change the default password
4. Generate or install an SSH key to use for authentication
5. Reconfigure /etc/ssh/sshd_config as follows:
  * Uncomment ListenAddress and configure it to only listen on the WAN interface - This is to prevent any chance of having a malicious VM access the running sshd even if iptables fails
  * Uncomment PasswordAuthentication and set it to yes - We will never not be using a key to authenticate
6. /etc/init.d/sshd reload - Confirm access is retained
7. Install required software: `apt-get install git tor`
  * git is used for easily installing and updating the freedumbhost sources
8. Install golang. We require a newer version than is found in Debian Jessie: https://golang.org/doc/install (1.5 upwards should work)
9. Configure tor:
  * We will replace our torrc with one from freedumbhost - https://raw.githubusercontent.com/freedumbhost/torhost-control/master/torcontrol-daemon/assets/torrc
  * Edit the torrc to remove all lines from `## Manual Configuration` onwards, as this is not required until our daemon is running properly
  * Verify tor is able to run with this new configuration: `/etc/init.d/tor restart && sleep 5 && ps aux | grep tor`
  * Enable tor starting on boot: `update-rc.d tor defaults`
10. Configure our network interfaces:
  * Replace existing configuration for eth0 as follows to `/etc/network/interfaces`:
```
auto eth0
iface eth0 inet static
	address 10.0.0.5
	netmask 255.255.255.0
	gateway 0.0.0.0
``` 
11. Plug in the network cable for eth0, to the hypervisor
12. Though this step may not be required, it is good practice to reboot the pi here to ensure a consistent and working network configuration.
13. If the hypervisor is running, confirm connectivity by running `ping 10.0.0.20`

*As pi*
14. Clone the required sources: `git clone https://github.com/freedumbhost/torhost-control.git`

