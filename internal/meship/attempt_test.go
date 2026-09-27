package meship

import (
	"net"
	"strings"
	"testing"
)

// TestIPForNameAttemptMatchesWireKube pins this package against WireKube's
// pkg/meship, which is the only thing that makes the copy safe. The vectors
// were generated from WireKube's implementation; if either side changes its
// hash, its stride, or its handling of the network and broadcast addresses,
// these fail rather than the two silently handing different addresses to
// kubelet and to the mesh.
func TestIPForNameAttemptMatchesWireKube(t *testing.T) {
	for _, c := range []struct {
		meshCIDR string
		name     string
		attempt  int
		want     string
	}{
		{"198.18.18.0/24", "master", 0, "198.18.18.74/32"},
		{"198.18.18.0/24", "master", 1, "198.18.18.55/32"},
		{"198.18.18.0/24", "master", 2, "198.18.18.36/32"},
		{"198.18.18.0/24", "master", 3, "198.18.18.17/32"},
		{"198.18.18.0/24", "master", 7, "198.18.18.195/32"},
		{"198.18.18.0/24", "master", 31, "198.18.18.247/32"},
		{"198.18.18.0/24", "master", 32, "198.18.18.228/32"},
		{"198.18.18.0/24", "master", 253, "198.18.18.93/32"},
		{"198.18.18.0/24", "master", 254, "198.18.18.74/32"},
		{"198.18.18.0/24", "master", 1000, "198.18.18.124/32"},
		{"198.18.18.0/24", "worker1", 0, "198.18.18.83/32"},
		{"198.18.18.0/24", "worker1", 1, "198.18.18.138/32"},
		{"198.18.18.0/24", "worker1", 2, "198.18.18.193/32"},
		{"198.18.18.0/24", "worker1", 3, "198.18.18.248/32"},
		{"198.18.18.0/24", "worker1", 7, "198.18.18.214/32"},
		{"198.18.18.0/24", "worker1", 31, "198.18.18.10/32"},
		{"198.18.18.0/24", "worker1", 32, "198.18.18.65/32"},
		{"198.18.18.0/24", "worker1", 253, "198.18.18.28/32"},
		{"198.18.18.0/24", "worker1", 254, "198.18.18.83/32"},
		{"198.18.18.0/24", "worker1", 1000, "198.18.18.219/32"},
		{"198.18.18.0/24", "win-e2e", 0, "198.18.18.17/32"},
		{"198.18.18.0/24", "win-e2e", 1, "198.18.18.232/32"},
		{"198.18.18.0/24", "win-e2e", 2, "198.18.18.193/32"},
		{"198.18.18.0/24", "win-e2e", 3, "198.18.18.154/32"},
		{"198.18.18.0/24", "win-e2e", 7, "198.18.18.252/32"},
		{"198.18.18.0/24", "win-e2e", 31, "198.18.18.78/32"},
		{"198.18.18.0/24", "win-e2e", 32, "198.18.18.39/32"},
		{"198.18.18.0/24", "win-e2e", 253, "198.18.18.56/32"},
		{"198.18.18.0/24", "win-e2e", 254, "198.18.18.17/32"},
		{"198.18.18.0/24", "win-e2e", 1000, "198.18.18.133/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 0, "198.18.18.117/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 1, "198.18.18.36/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 2, "198.18.18.209/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 3, "198.18.18.128/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 7, "198.18.18.58/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 31, "198.18.18.146/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 32, "198.18.18.65/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 253, "198.18.18.198/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 254, "198.18.18.117/32"},
		{"198.18.18.0/24", "idleloom-e2e-linux", 1000, "198.18.18.143/32"},
		{"198.18.18.0/24", "", 0, "198.18.18.132/32"},
		{"198.18.18.0/24", "", 1, "198.18.18.119/32"},
		{"198.18.18.0/24", "", 2, "198.18.18.106/32"},
		{"198.18.18.0/24", "", 3, "198.18.18.93/32"},
		{"198.18.18.0/24", "", 7, "198.18.18.41/32"},
		{"198.18.18.0/24", "", 31, "198.18.18.237/32"},
		{"198.18.18.0/24", "", 32, "198.18.18.224/32"},
		{"198.18.18.0/24", "", 253, "198.18.18.145/32"},
		{"198.18.18.0/24", "", 254, "198.18.18.132/32"},
		{"198.18.18.0/24", "", 1000, "198.18.18.86/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 0, "198.18.18.231/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 1, "198.18.18.212/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 2, "198.18.18.193/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 3, "198.18.18.174/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 7, "198.18.18.98/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 31, "198.18.18.150/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 32, "198.18.18.131/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 253, "198.18.18.250/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 254, "198.18.18.231/32"},
		{"198.18.18.0/24", "a-very-long-node-name-that-goes-on", 1000, "198.18.18.27/32"},
		{"100.64.0.0/10", "master", 0, "100.77.197.136/32"},
		{"100.64.0.0/10", "master", 1, "100.102.213.161/32"},
		{"100.64.0.0/10", "master", 2, "100.127.229.186/32"},
		{"100.64.0.0/10", "master", 3, "100.88.245.213/32"},
		{"100.64.0.0/10", "master", 7, "100.125.54.59/32"},
		{"100.64.0.0/10", "master", 31, "100.86.184.167/32"},
		{"100.64.0.0/10", "master", 32, "100.111.200.192/32"},
		{"100.64.0.0/10", "master", 253, "100.82.175.3/32"},
		{"100.64.0.0/10", "master", 254, "100.107.191.28/32"},
		{"100.64.0.0/10", "master", 1000, "100.116.170.62/32"},
		{"100.64.0.0/10", "worker1", 0, "100.97.55.95/32"},
		{"100.64.0.0/10", "worker1", 1, "100.100.75.24/32"},
		{"100.64.0.0/10", "worker1", 2, "100.103.94.209/32"},
		{"100.64.0.0/10", "worker1", 3, "100.106.114.138/32"},
		{"100.64.0.0/10", "worker1", 7, "100.118.193.110/32"},
		{"100.64.0.0/10", "worker1", 31, "100.64.154.202/32"},
		{"100.64.0.0/10", "worker1", 32, "100.67.174.131/32"},
		{"100.64.0.0/10", "worker1", 253, "100.107.181.76/32"},
		{"100.64.0.0/10", "worker1", 254, "100.110.201.5/32"},
		{"100.64.0.0/10", "worker1", 1000, "100.102.66.103/32"},
		{"100.64.0.0/10", "win-e2e", 0, "100.72.182.129/32"},
		{"100.64.0.0/10", "win-e2e", 1, "100.111.15.46/32"},
		{"100.64.0.0/10", "win-e2e", 2, "100.85.103.221/32"},
		{"100.64.0.0/10", "win-e2e", 3, "100.123.192.138/32"},
		{"100.64.0.0/10", "win-e2e", 7, "100.85.35.68/32"},
		{"100.64.0.0/10", "win-e2e", 31, "100.109.115.152/32"},
		{"100.64.0.0/10", "win-e2e", 32, "100.83.204.71/32"},
		{"100.64.0.0/10", "win-e2e", 253, "100.110.90.168/32"},
		{"100.64.0.0/10", "win-e2e", 254, "100.84.179.87/32"},
		{"100.64.0.0/10", "win-e2e", 1000, "100.83.30.247/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 0, "100.113.14.147/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 1, "100.103.9.182/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 2, "100.93.4.217/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 3, "100.82.255.252/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 7, "100.106.236.134/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 31, "100.122.119.198/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 32, "100.112.114.233/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 253, "100.74.63.220/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 254, "100.64.58.255/32"},
		{"100.64.0.0/10", "idleloom-e2e-linux", 1000, "100.78.14.19/32"},
		{"100.64.0.0/10", "", 0, "100.92.161.206/32"},
		{"100.64.0.0/10", "", 1, "100.81.176.99/32"},
		{"100.64.0.0/10", "", 2, "100.70.190.248/32"},
		{"100.64.0.0/10", "", 3, "100.123.205.139/32"},
		{"100.64.0.0/10", "", 7, "100.80.7.223/32"},
		{"100.64.0.0/10", "", 31, "100.73.101.207/32"},
		{"100.64.0.0/10", "", 32, "100.126.116.98/32"},
		{"100.64.0.0/10", "", 253, "100.76.10.185/32"},
		{"100.64.0.0/10", "", 254, "100.65.25.78/32"},
		{"100.64.0.0/10", "", 1000, "100.93.150.128/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 0, "100.118.150.223/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 1, "100.123.215.250/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 2, "100.65.25.23/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 3, "100.70.90.50/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 7, "100.91.94.158/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 31, "100.89.121.42/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 32, "100.94.186.69/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 253, "100.103.238.184/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 254, "100.109.47.211/32"},
		{"100.64.0.0/10", "a-very-long-node-name-that-goes-on", 1000, "100.124.232.251/32"},
		{"10.0.0.0/30", "master", 0, "10.0.0.2/32"},
		{"10.0.0.0/30", "master", 1, "10.0.0.1/32"},
		{"10.0.0.0/30", "master", 2, "10.0.0.2/32"},
		{"10.0.0.0/30", "master", 3, "10.0.0.1/32"},
		{"10.0.0.0/30", "master", 7, "10.0.0.1/32"},
		{"10.0.0.0/30", "master", 31, "10.0.0.1/32"},
		{"10.0.0.0/30", "master", 32, "10.0.0.2/32"},
		{"10.0.0.0/30", "master", 253, "10.0.0.1/32"},
		{"10.0.0.0/30", "master", 254, "10.0.0.2/32"},
		{"10.0.0.0/30", "master", 1000, "10.0.0.2/32"},
		{"10.0.0.0/30", "worker1", 0, "10.0.0.1/32"},
		{"10.0.0.0/30", "worker1", 1, "10.0.0.2/32"},
		{"10.0.0.0/30", "worker1", 2, "10.0.0.1/32"},
		{"10.0.0.0/30", "worker1", 3, "10.0.0.2/32"},
		{"10.0.0.0/30", "worker1", 7, "10.0.0.2/32"},
		{"10.0.0.0/30", "worker1", 31, "10.0.0.2/32"},
		{"10.0.0.0/30", "worker1", 32, "10.0.0.1/32"},
		{"10.0.0.0/30", "worker1", 253, "10.0.0.2/32"},
		{"10.0.0.0/30", "worker1", 254, "10.0.0.1/32"},
		{"10.0.0.0/30", "worker1", 1000, "10.0.0.1/32"},
		{"10.0.0.0/30", "win-e2e", 0, "10.0.0.1/32"},
		{"10.0.0.0/30", "win-e2e", 1, "10.0.0.2/32"},
		{"10.0.0.0/30", "win-e2e", 2, "10.0.0.1/32"},
		{"10.0.0.0/30", "win-e2e", 3, "10.0.0.2/32"},
		{"10.0.0.0/30", "win-e2e", 7, "10.0.0.2/32"},
		{"10.0.0.0/30", "win-e2e", 31, "10.0.0.2/32"},
		{"10.0.0.0/30", "win-e2e", 32, "10.0.0.1/32"},
		{"10.0.0.0/30", "win-e2e", 253, "10.0.0.2/32"},
		{"10.0.0.0/30", "win-e2e", 254, "10.0.0.1/32"},
		{"10.0.0.0/30", "win-e2e", 1000, "10.0.0.1/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 0, "10.0.0.1/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 1, "10.0.0.2/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 2, "10.0.0.1/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 3, "10.0.0.2/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 7, "10.0.0.2/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 31, "10.0.0.2/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 32, "10.0.0.1/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 253, "10.0.0.2/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 254, "10.0.0.1/32"},
		{"10.0.0.0/30", "idleloom-e2e-linux", 1000, "10.0.0.1/32"},
		{"10.0.0.0/30", "", 0, "10.0.0.2/32"},
		{"10.0.0.0/30", "", 1, "10.0.0.1/32"},
		{"10.0.0.0/30", "", 2, "10.0.0.2/32"},
		{"10.0.0.0/30", "", 3, "10.0.0.1/32"},
		{"10.0.0.0/30", "", 7, "10.0.0.1/32"},
		{"10.0.0.0/30", "", 31, "10.0.0.1/32"},
		{"10.0.0.0/30", "", 32, "10.0.0.2/32"},
		{"10.0.0.0/30", "", 253, "10.0.0.1/32"},
		{"10.0.0.0/30", "", 254, "10.0.0.2/32"},
		{"10.0.0.0/30", "", 1000, "10.0.0.2/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 0, "10.0.0.1/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 1, "10.0.0.2/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 2, "10.0.0.1/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 3, "10.0.0.2/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 7, "10.0.0.2/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 31, "10.0.0.2/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 32, "10.0.0.1/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 253, "10.0.0.2/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 254, "10.0.0.1/32"},
		{"10.0.0.0/30", "a-very-long-node-name-that-goes-on", 1000, "10.0.0.1/32"},
		{"172.16.0.0/20", "master", 0, "172.16.4.22/32"},
		{"172.16.0.0/20", "master", 1, "172.16.11.27/32"},
		{"172.16.0.0/20", "master", 2, "172.16.2.34/32"},
		{"172.16.0.0/20", "master", 3, "172.16.9.39/32"},
		{"172.16.0.0/20", "master", 7, "172.16.5.63/32"},
		{"172.16.0.0/20", "master", 31, "172.16.13.203/32"},
		{"172.16.0.0/20", "master", 32, "172.16.4.210/32"},
		{"172.16.0.0/20", "master", 253, "172.16.4.229/32"},
		{"172.16.0.0/20", "master", 254, "172.16.11.234/32"},
		{"172.16.0.0/20", "master", 1000, "172.16.3.12/32"},
		{"172.16.0.0/20", "worker1", 0, "172.16.10.159/32"},
		{"172.16.0.0/20", "worker1", 1, "172.16.2.164/32"},
		{"172.16.0.0/20", "worker1", 2, "172.16.10.167/32"},
		{"172.16.0.0/20", "worker1", 3, "172.16.2.172/32"},
		{"172.16.0.0/20", "worker1", 7, "172.16.2.188/32"},
		{"172.16.0.0/20", "worker1", 31, "172.16.3.28/32"},
		{"172.16.0.0/20", "worker1", 32, "172.16.11.31/32"},
		{"172.16.0.0/20", "worker1", 253, "172.16.6.148/32"},
		{"172.16.0.0/20", "worker1", 254, "172.16.14.151/32"},
		{"172.16.0.0/20", "worker1", 1000, "172.16.10.65/32"},
		{"172.16.0.0/20", "win-e2e", 0, "172.16.15.91/32"},
		{"172.16.0.0/20", "win-e2e", 1, "172.16.5.152/32"},
		{"172.16.0.0/20", "win-e2e", 2, "172.16.11.211/32"},
		{"172.16.0.0/20", "win-e2e", 3, "172.16.2.16/32"},
		{"172.16.0.0/20", "win-e2e", 7, "172.16.10.254/32"},
		{"172.16.0.0/20", "win-e2e", 31, "172.16.0.154/32"},
		{"172.16.0.0/20", "win-e2e", 32, "172.16.6.213/32"},
		{"172.16.0.0/20", "win-e2e", 253, "172.16.8.112/32"},
		{"172.16.0.0/20", "win-e2e", 254, "172.16.14.171/32"},
		{"172.16.0.0/20", "win-e2e", 1000, "172.16.8.223/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 0, "172.16.2.121/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 1, "172.16.13.30/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 2, "172.16.7.197/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 3, "172.16.2.108/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 7, "172.16.13.4/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 31, "172.16.12.156/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 32, "172.16.7.67/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 253, "172.16.8.218/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 254, "172.16.3.129/32"},
		{"172.16.0.0/20", "idleloom-e2e-linux", 1000, "172.16.12.51/32"},
		{"172.16.0.0/20", "", 0, "172.16.3.94/32"},
		{"172.16.0.0/20", "", 1, "172.16.9.167/32"},
		{"172.16.0.0/20", "", 2, "172.16.15.240/32"},
		{"172.16.0.0/20", "", 3, "172.16.6.59/32"},
		{"172.16.0.0/20", "", 7, "172.16.15.97/32"},
		{"172.16.0.0/20", "", 31, "172.16.6.77/32"},
		{"172.16.0.0/20", "", 32, "172.16.12.150/32"},
		{"172.16.0.0/20", "", 253, "172.16.10.73/32"},
		{"172.16.0.0/20", "", 254, "172.16.0.148/32"},
		{"172.16.0.0/20", "", 1000, "172.16.3.152/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 0, "172.16.5.55/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 1, "172.16.14.10/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 2, "172.16.6.223/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 3, "172.16.15.178/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 7, "172.16.3.4/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 31, "172.16.6.230/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 32, "172.16.15.185/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 253, "172.16.14.212/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 254, "172.16.7.169/32"},
		{"172.16.0.0/20", "a-very-long-node-name-that-goes-on", 1000, "172.16.1.191/32"},
	} {
		got, err := IPForNameAttempt(c.name, c.meshCIDR, c.attempt)
		if err != nil {
			t.Fatalf("IPForNameAttempt(%q, %q, %d): %v", c.name, c.meshCIDR, c.attempt, err)
		}
		if got != c.want {
			t.Errorf("IPForNameAttempt(%q, %q, %d) = %s, want %s", c.name, c.meshCIDR, c.attempt, got, c.want)
		}
	}
}

// TestAttemptsVisitEveryAddressOnce is the property the claim walk depends on:
// a caller that keeps incrementing never re-probes an address it already ruled
// out, and reaches every free one.
func TestAttemptsVisitEveryAddressOnce(t *testing.T) {
	for _, meshCIDR := range []string{"10.0.0.0/30", "10.0.0.0/29", "10.1.2.0/28", "198.18.18.0/24"} {
		capacity, err := Capacity(meshCIDR)
		if err != nil {
			t.Fatalf("Capacity(%q): %v", meshCIDR, err)
		}
		for _, name := range []string{"win-e2e", "idleloom-e2e-linux", "worker1", ""} {
			seen := make(map[string]int, capacity)
			for attempt := range capacity {
				got, err := IPForNameAttempt(name, meshCIDR, attempt)
				if err != nil {
					t.Fatal(err)
				}
				if prev, ok := seen[got]; ok {
					t.Fatalf("%q in %s: attempts %d and %d both → %s", name, meshCIDR, prev, attempt, got)
				}
				seen[got] = attempt
				if !Contains(got, meshCIDR) {
					t.Fatalf("%q in %s attempt %d: %s is not an address the CIDR can hand out", name, meshCIDR, attempt, got)
				}
			}
			if len(seen) != capacity {
				t.Fatalf("%q in %s covered %d of %d addresses", name, meshCIDR, len(seen), capacity)
			}
		}
	}
}

func TestAttemptsWrapAtCapacity(t *testing.T) {
	const meshCIDR = "198.18.18.0/24"
	capacity, err := Capacity(meshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []int{0, 1, 37, capacity - 1} {
		want, err := IPForNameAttempt("worker1", meshCIDR, attempt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := IPForNameAttempt("worker1", meshCIDR, attempt+capacity)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("attempt %d wrapped to %s, want %s", attempt+capacity, got, want)
		}
	}
}

// TestLargeAttemptDoesNotOverflow guards the uint64 widening: a 32-bit
// multiply of stride by attempt would wrap and leave the sequence.
func TestLargeAttemptDoesNotOverflow(t *testing.T) {
	const meshCIDR = "100.64.0.0/10"
	capacity, err := Capacity(meshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []int{capacity - 1, capacity, 1 << 30, 1<<31 - 1} {
		got, err := IPForNameAttempt("worker1", meshCIDR, attempt)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if !Contains(got, meshCIDR) {
			t.Fatalf("attempt %d produced %s, outside %s", attempt, got, meshCIDR)
		}
		want, err := IPForNameAttempt("worker1", meshCIDR, attempt%capacity)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("attempt %d = %s, want the wrapped %s", attempt, got, want)
		}
	}
}

func TestIPForNameAttemptRejectsNegative(t *testing.T) {
	if _, err := IPForNameAttempt("worker1", "198.18.18.0/24", -1); err == nil {
		t.Fatal("expected an error for a negative attempt")
	}
}

func TestCapacity(t *testing.T) {
	for meshCIDR, want := range map[string]int{
		"10.0.0.0/30":    2,
		"198.18.18.0/24": 254,
		"100.64.0.0/10":  4194302,
	} {
		got, err := Capacity(meshCIDR)
		if err != nil {
			t.Fatalf("Capacity(%q): %v", meshCIDR, err)
		}
		if got != want {
			t.Errorf("Capacity(%q) = %d, want %d", meshCIDR, got, want)
		}
	}
}

func TestContains(t *testing.T) {
	const meshCIDR = "198.18.18.0/24"
	for _, c := range []struct {
		address string
		want    bool
	}{
		{"198.18.18.1/32", true},
		{"198.18.18.254/32", true},
		{"198.18.18.0/32", false},
		{"198.18.18.255/32", false},
		{"198.18.19.1/32", false},
		{"198.18.18.1/24", false},
		{"198.18.18.1", false},
		{"fd00::1/128", false},
		{"", false},
	} {
		if got := Contains(c.address, meshCIDR); got != c.want {
			t.Errorf("Contains(%q) = %v, want %v", c.address, got, c.want)
		}
	}
	for _, meshCIDR := range []string{"", "not-a-cidr", "2001:db8::/32", "10.0.0.0/31", "0.0.0.0/0"} {
		if Contains("198.18.18.1/32", meshCIDR) {
			t.Errorf("Contains accepted mesh CIDR %q", meshCIDR)
		}
	}
}

// TestRejectsUnusableCIDRs includes /0, which shifts out of a 32-bit size and
// would otherwise wrap into a bogus offset.
func TestRejectsUnusableCIDRs(t *testing.T) {
	for _, meshCIDR := range []string{"", "not-a-cidr", "2001:db8::/32", "10.0.0.0/31", "10.0.0.0/32", "0.0.0.0/0"} {
		if _, err := IPForNameAttempt("worker1", meshCIDR, 0); err == nil {
			t.Errorf("IPForNameAttempt accepted mesh CIDR %q", meshCIDR)
		}
		if _, err := Capacity(meshCIDR); err == nil {
			t.Errorf("Capacity accepted mesh CIDR %q", meshCIDR)
		}
	}
}

// TestAddressForNameAttemptDropsThePrefixLength keeps the kubelet --node-ip
// form in step with the CIDR form.
func TestAddressForNameAttemptDropsThePrefixLength(t *testing.T) {
	for _, attempt := range []int{0, 1, 9} {
		full, err := IPForNameAttempt("worker1", "198.18.18.0/24", attempt)
		if err != nil {
			t.Fatal(err)
		}
		bare, err := AddressForNameAttempt("worker1", "198.18.18.0/24", attempt)
		if err != nil {
			t.Fatal(err)
		}
		if bare != strings.TrimSuffix(full, "/32") {
			t.Errorf("attempt %d: AddressForNameAttempt = %s, IPForNameAttempt = %s", attempt, bare, full)
		}
		if net.ParseIP(bare) == nil {
			t.Errorf("attempt %d: %q does not parse as an address", attempt, bare)
		}
	}
}
