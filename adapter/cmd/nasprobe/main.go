// nasprobe：NAS 连通性诊断工具。
//
// 用于定位「curl 能通、Go 客户端报 EOF」这类问题，分三步探测：
//  1. 裸 TCP 连接（能否建立）
//  2. TLS 握手（协商出的版本/套件，失败点在哪）
//  3. 完整 HTTPS 请求（实际业务路径）
//
// 用法：
//   go run ./cmd/nasprobe -url https://192.168.10.204:5001
//   go run ./cmd/nasprobe -url https://192.168.10.204:5001 -path /webapi/query.cgi
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	base := flag.String("url", "", "NAS 地址，如 https://192.168.10.204:5001")
	path := flag.String("path", "/webapi/auth.cgi?api=SYNO.API.Auth&version=6&method=login&account=probe&passwd=probe&session=FileStation&format=sid", "请求路径（含 query）")
	http1 := flag.Bool("http1", false, "强制 HTTP/1.1（禁用 HTTP/2 协商），用于排查 ALPN h2 导致的 EOF")
	repeat := flag.Int("repeat", 0, "重复做 TLS 握手 N 次，统计失败率（0=不重复）")
	flag.Parse()

	if *base == "" {
		fmt.Fprintln(os.Stderr, "必须指定 -url")
		os.Exit(2)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(*base, "https://"), "http://")
	host = strings.TrimRight(host, "/")

	// ---- 1. 裸 TCP ----
	fmt.Printf("[1] TCP 连接 %s ...\n", host)
	d := &net.Dialer{Timeout: 8 * time.Second}
	tcp, err := d.Dial("tcp", host)
	if err != nil {
		fmt.Printf("    ✗ 失败: %v\n", err)
		os.Exit(1)
	}
	local := tcp.LocalAddr().String()
	remote := tcp.RemoteAddr().String()
	fmt.Printf("    ✓ 已连接 (local=%s remote=%s)\n", local, remote)
	tcp.Close()

	// ---- 2. TLS 握手 ----
	fmt.Printf("[2] TLS 握手 ...\n")
	td := &tls.Dialer{
		NetDialer: d,
		Config:    &tls.Config{InsecureSkipVerify: true},
	}
	conn, err := td.Dial("tcp", host)
	if err != nil {
		fmt.Printf("    ✗ 握手失败: %T %v\n", err, err)
		os.Exit(1)
	}
	st := conn.(*tls.Conn).ConnectionState()
	fmt.Printf("    ✓ 握手成功: 版本=%s 套件=%s 服务端证书数=%d\n",
		tlsName(st.Version), tls.CipherSuiteName(st.CipherSuite), len(st.PeerCertificates))
	if len(st.PeerCertificates) > 0 {
		fmt.Printf("      服务端证书 CN=%s 签发者=%s\n",
			st.PeerCertificates[0].Subject.CommonName,
			st.PeerCertificates[0].Issuer.CommonName)
	}
	conn.Close()

	// ---- 2b. 重复握手统计（判断是偶发还是必现）----
	if *repeat > 0 {
		fmt.Printf("[2b] 重复 TLS 握手 %d 次 ...\n", *repeat)
		okN, failN := 0, 0
		var firstErr error
		for i := 0; i < *repeat; i++ {
			c, err := td.Dial("tcp", host)
			if err != nil {
				failN++
				if firstErr == nil {
					firstErr = err
				}
				fmt.Printf("    #%d ✗ %v\n", i+1, err)
				continue
			}
			okN++
			c.Close()
		}
		fmt.Printf("    成功 %d / 失败 %d（失败率 %.0f%%）\n",
			okN, failN, float64(failN)*100/float64(*repeat))
		if firstErr != nil {
			fmt.Printf("    首个错误: %T %v\n", firstErr, firstErr)
		}
	}

	// ---- 3. 完整 HTTPS 请求 ----
	full := strings.TrimRight(*base, "/") + *path
	fmt.Printf("[3] HTTPS 请求 %s\n", truncate(full, 120))
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	if *http1 {
		client.Transport.(*http.Transport).ForceAttemptHTTP2 = false
		client.Transport.(*http.Transport).TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		fmt.Println("    (已强制 HTTP/1.1)")
	}
	req, _ := http.NewRequest("GET", full, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("    ✗ 请求失败: %T %v\n", err, err)
		if ue, ok := err.(*url.Error); ok {
			fmt.Printf("      url.Error: op=%s err=%T %v\n", ue.Op, ue.Err, ue.Err)
		}
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	fmt.Printf("    ✓ HTTP %d %s\n", resp.StatusCode, resp.Status)
	fmt.Printf("      body: %s\n", truncate(strings.TrimSpace(string(body)), 400))
}

func tlsName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	}
	return fmt.Sprintf("未知(%#x)", v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
