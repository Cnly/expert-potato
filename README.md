# ?

<br>

## Introduction

This project does not have a name yet. `expert-potato` is just a name suggested by GitHub on creation on this repository. The project has a codename, `NEO-181`, though.

This project is originally created to bypass network speed limitation between LAN and an external host for one special case, where:

- one has multiple LAN IPs, and
- one's LAN limits network speed on a per-IP basis

<br>

## How it Works

In short, this app acts like a tunnel that spread data stream over the multiple LAN IPs one has, to achieve the goal of multiplying the network bandwidth. This may have some similarities to [MPTCP](https://www.multipath-tcp.org/).

Currently only TCP is supported.

**Still interested in more details? See below.**

Four basic components of this app:

- **EConn** (i.e. **End Connection**, the connection between this app and other apps)
- **ECM** (i.e. **EConn Manager**)
- **PConn** (i.e. **Parallel Connection**, the connection(s) between client and server of this app)
- **PCM** (i.e. **PConn Manager**)

Both ECM and PCM can run in client mode or server mode, respectively. (i.e. It is possible to have ECM in server mode and PCM in client mode on one host.)

ECM, when running in client mode, creates a listening socket. When a connection is established with the listening socket, ECM client allocates an ID for the connection, and get prepared to forward data to ECM server (through PCM). When running in server mode, ECM receives data from ECM client (through PCM), and forward data to the pre-configured address. The most significant difference between ECM client and server is client listens for connections, while server establish connections to the pre-configured address on receving request from ECM client.

PCM, when running in client mode, creates and maintains one (persistent) TCP connection to PCM server, on every binding IP configured. PCM server listens and accepts PConns. Data streams are spread over all valid PConns.

<br>

// To be completed
