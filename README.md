# rport
Create reverse tunnels with ease.

## At a glance
Rport helps you to manage your remote servers without the hassle of VPNs, chained SSH connections, jump-hosts, or the use of commercial tools like TeamViewer and its clones. 

Rport acts as server and client establishing permanent or on-demand secure tunnels to devices inside protected intranets behind a firewall. 

All operating systems provide secure and well-established mechanisms for remote management, being SSH and Remote Desktop the most widely used. Rport makes them accessible easily and securely. 

**Is Rport a replacement for TeamViewer?**
Yes and no. It depends on your needs.
TeamViewer and a couple of similar products are focused on giving access to a remote graphical desktop bypassing the Remote Desktop implementation of Microsoft. They fall short in a heterogeneous environment where access to headless Linux machines is needed. But they are without alternatives for Windows Home Editions.
Apart from remote management, they offer supplementary services like Video Conferences, desktop sharing, screen mirroring, or spontaneous remote assistance for desktop users.

**Goal of Rport**
Rport focusses only on remote management of those operating systems where an existing login mechanism can be used. It can be used for Linux and Windows, but also appliances and IoT devices providing a web-based configuration. 
From a technological perspective, [Ngork](https://ngrok.com/) and [openport.io](https://openport.io) are similar products. Rport differs from them in many aspects.
* Rport is 100% open source. Client and Server. Remote management is a matter of trust and security. Rport is fully transparent.
* Rport will come with a user interface making the management of remote systems easy and user-friendly.
* Rport is made for all operating systems with native and small binaries. No need for Python or similar heavyweights.
* Rport allows you to self-host the server.
* Rport allows clients to wait in standby mode without an active tunnel. Tunnels can be requested on-demand by the user remotely.


## Build and installation
We provide [pre-compiled binaries](https://github.com/cloudradar-monitoring/rport/releases).
### From source
1) Build from source (Linux or Mac OS/X.):
    ```bash
    make all
    ```
    `rport` and `rportd` binaries will appear in directory.  

2) Build using Docker:
    ```bash
    make docker-goreleaser
    ```
    will create binaries for all supported platforms in ./dist directory.

## Usage
`rportd` should be executed on the machine, acting as a server.

`rport` is a client app which will try to establish long-running connection to the server.

Minimal setup:
1) Execute `./rportd -p 9999` on a server.
1) Execute `./rport <SERVER_IP>:9999 3389:3389` on a client.
1) Now end-users can connect to `<SERVER_IP>:3389` (e.g. using Remote Desktop Connection). The connection will be proxied to client machine.

See `./rportd --help` and `./rport --help` for more options, like:
- Specifying certificate fingerprint to validate server authority
- Client session authentication using user:password pair
- Restricting, which users can connect
- Specifying additional intermediate HTTP proxy
- Using POSIX signals to control running apps
- Setting custom HTTP headers

## Quickstart guide
### Install and run the rport server
On a machine connected to the public internet and ideally with an FQDN registered to a public DNS install and run the server.
The server is called node1.example.com in this example.

Install client and server
```
curl -LSs https://github.com/cloudradar-monitoring/rport/releases/download/0.1.0/rport_0.1.0-SNAPSHOT-7323e7c_linux_amd64.tar.gz|\
tar vxzf - -C /usr/local/bin/
````

Create a key for the server instance. Store this key and don't change it. Otherwise, your fingerprint will change and your clients might be rejected. 
```
openssl rand -hex 18
```  

Start the server as a background task.
```
nohup rportd --key <YOUR_KEY> -p 19075 &>/tmp/rportd.log &
```
For the first testing leave the console open and observe the log with `tail -f /tmp/rportd.log`. Note the fingerprint. You will use it later. 

To safely store and reuse the key use these commands.
```
echo "RPORT_KEY=$(openssl rand -hex 18)">/etc/default/rport
. /etc/default/rport
export RPORT_KEY=$RPORT_KEY
nohup rportd -p 19075 &>/tmp/rportd.log &
```
rportd reads the key from the environment so it does not appear in the process list or the history. 

### Connect a client
We call the client `client1.local.localdomain`.
On your client just install the client binary 
```
curl -LSs https://github.com/cloudradar-monitoring/rport/releases/download/0.1.0/rport_0.1.0-SNAPSHOT-7323e7c_linux_amd64.tar.gz|\
tar vxzf - rport -C /usr/local/bin/
```

Create an ad hoc tunnel that will forward the port 2222 of node1.example.com to the to local port 22 of client1.local.localdomain.
`rport node1.example.com:19075 2222:0.0.0.0:22`
Observing the log of the server you get a confirmation about the newly created tunnel.

Now you can access your machine behind a firewall through the tunnel. Try `ssh -p 2222 node1.example.com` and you will come out on the machine where the tunnel has been initiated.

#### Let's improve security by using fingerprints
Copy the fingerprint the server has generated on startup to your clipboard and use it on the client like this 
`rport --fingerprint <YOUR_FINGERPRINT> node1.example.com:19075 2222:0.0.0.0:22`.

This ensures you connect only to trusted servers. If you omit this step a man in the middle can bring up a rport server and hijack your tunnels.
If you do ssh or rdp through the tunnel, a hijacked tunnel will not expose your credentials because the data inside the tunnel is still encrypted. But if you use rport for unencrypted protocols like HTTP, sniffing credentials would be possible.

### Using systemd
Packages for most common distributions and Windows are on our roadmap. In the meantime create a systemd service file in `/etc/systemd/system/rportd.service` with the following lines manually.
``` 
[Unit]
Description=Rport Server Daemon
After=network-online.target
Wants=network-online.target systemd-networkd-wait-online.service

[Service]
User=rport
Group=rport
WorkingDirectory=/var/lib/rport/
EnvironmentFile=/etc/default/rportd
ExecStart=/usr/local/bin/rportd
Restart=on-failure
RestartSec=5
StandardOutput=file:/var/log/rportd/rportd.log
StandardError=file:/var/log/rportd/rportd.log

[Install]
WantedBy=multi-user.target
```

Create a user because rport has no requirement to run as root
```
useradd -m -r -s /bin/false -d /var/lib/rport rport
mkdir /var/log/rportd
chown rport:root /var/log/rportd
```

Create a config file `/etc/default/rport` like this example.
```
RPORT_KEY=<YOUR_KEY>
HOST=0.0.0.0
PORT=19075
```

Start it
```
systemctl daemon-reload
service rportd restart
```

### Using authentication
Anyone who knows the address and the port of your rport server can use it for tunneling. In most cases, this is not desired. Your rport server could be abused for example to publish content under your IP address. Therefore using rport with authentication is highly recommended.

For the server `rportd --auth rport:password123` is the most basic option. All clients must use the username `rport` and the given password. 

On the client start the tunnel this way
`rport --auth rport:password123 --fingerprint <YOUR_FINGERPRINT> node2.rport.io:19075 2222:0.0.0.0:22`
*Note that in this early version the order of the command line options is still important. This might change later.*

If you want to maintain multiple users with different passwords, create a json-file `/etc/rportd-auth.json` with credentials, for example
```
{
    "user1:foobaz": [
        ".*"
    ],
    "user2:bingo": [
        "210\\.211\\.212.*",
        "107\\.108\\.109.*"
    ],
    "rport:password123": [
        "^999"
    ]
}
```
*For now, rportd reads only the user and password. The optional filters to limit the tunnels to match a regex are under construction.*
*Rportd reads the file immediately after writing without the need for a sighub. This might change in the future.*

Start the server with `rportd --authfile /etc/rport-auth.json`. Change the `ExecStart` line of the systemd service file accordingly. 

### On-demand tunnels
Initializing the creation of a tunnel from the client is nice but not a perfect solution for secure and reliable remote access to a large number of machines.
Most of the time the tunnel wouldn't be used. Network resources would be wasted and a port is exposed to the internet for an unnecessarily long time.
Rport provides the option to establish tunnels from the server only when you need them.

Invoke the client without specifying a tunnel.  
```
rport node2.rport.io:19075
```
*Add auth and fingerprint as already explained.*

This attaches the client to the message queue of the server without creating a tunnel.
On the server, you can supervise the attached clients using 
`curl -s http://localhost:19075/api/v1/sessions`. *Use `jq` for pretty-printing json.*
Here is an example:
```
curl -s http://localhost:19075/api/v1/sessions|jq
[
  {
    "id": "b10a1419102708c1a8202eba0c2970f2e6410201c752fe7479724c52a8a137d9",
    "version": "0.1.0-SNAPSHOT-7323e7c",
    "address": "88.198.189.xxx:58354",
    "remotes": [
      {
        "lhost": "0.0.0.0",
        "lport": "2222",
        "rhost": "0.0.0.0",
        "rport": "22"
      }
    ]
  },
  {
    "id": "24fc518c23ddbacc109382c5b1b420c51ebb6b21ac214e095ef410c820ae6cd3",
    "version": "0.1.0-SNAPSHOT-7323e7c",
    "address": "88.198.189.xxx:54664",
    "remotes": []
  }
]
```
There is one client connected with an active tunnel. The second client is in standby mode.

Now use `PUT /api/v1/sessions/{id}/tunnels?local={port}&remote={port}` to request a new tunnel for a client session.
For example
```
ID=24fc518c23ddbacc109382c5b1b420c51ebb6b21ac214e095ef410c820ae6cd3
LOCAL_PORT=4000 
REMOTE_PORT=3389
curl -X PUT "http://localhost:19075/api/v1/sessions/$ID/tunnels?local=$LOCAL_PORT&remote=$REMOTE_PORT"
```
The ports are defined from the servers' perspective. The above example opens port 4000 on the rport server and forwards to the port 3389 of the client.

The API is very basic still. Authentication, a UI and many more options will follow soon. Stay connected with us.


### Versioning model
rport uses <major>.<minor>.<buildnumber> version pattern for compatibility with a maximum number of package managers.

Starting from version 1.0.0 packages with even <minor> number are considered stable.


### Credits
Forked from [jpillora/chisel](https://github.com/jpillora/chisel)
