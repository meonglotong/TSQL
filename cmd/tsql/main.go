// tsql is the TSQL command-line client.
//
// Usage:
//
//	tsql [-h host] [-p port] [-c "SQL"]     one-shot statement
//	tsql                                     interactive REPL (\q quit, \dt tables)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/meonglotong/tsql/internal/protocol"
)

func main() {
	cmd := flag.String("c", "", "run a single statement and exit")
	host := flag.String("h", "127.0.0.1", "server host")
	port := flag.Int("p", 5433, "server port")
	unixSock := flag.String("unix", "", "unix socket path (overrides -h/-p)")
	flag.Parse()

	var netName, addr string
	if *unixSock != "" {
		netName, addr = "unix", *unixSock
	} else {
		netName, addr = "tcp", fmt.Sprintf("%s:%d", *host, *port)
	}
	conn, err := net.DialTimeout(netName, addr, 5*1e9)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsql: connection to %s failed: %v\n", addr, err)
		os.Exit(2)
	}
	defer conn.Close()

	if *cmd != "" {
		os.Exit(runOnce(conn, *cmd))
	}
	repl(conn)
}

func runOnce(conn net.Conn, sql string) int {
	msgs, err := sendQuery(conn, sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
		return 1
	}
	if !printResults(os.Stdout, msgs) {
		return 1
	}
	return 0
}

// sendQuery writes one query frame and reads until the ready frame.
func sendQuery(conn net.Conn, sql string) ([]protocol.Message, error) {
	if err := protocol.WriteFrame(conn, protocol.Message{Type: "query", SQL: sql}); err != nil {
		return nil, err
	}
	var msgs []protocol.Message
	for {
		m, err := protocol.ReadFrame(conn)
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, m)
		if m.Type == "ready" {
			return msgs, nil
		}
	}
}

func printResults(w io.Writer, msgs []protocol.Message) bool {
	failed := false
	for _, m := range msgs {
		switch m.Type {
		case "result":
			if len(m.Rows) == 0 {
				fmt.Fprintln(w, "(0 rows)")
				continue
			}
			fmt.Fprint(w, formatTable(m.Columns, m.Rows))
			fmt.Fprintf(w, "(%d row%s)\n", len(m.Rows), plural(len(m.Rows)))
		case "ok":
			fmt.Fprintf(w, "OK (%d row%s)\n", m.Affected, plural(m.Affected))
		case "error":
			fmt.Fprintf(os.Stderr, "ERROR [%s]: %s\n", m.Code, m.Message)
			failed = true
		}
	}
	return !failed
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// formatTable renders psql-style aligned output.
func formatTable(cols []string, rows [][]any) string {
	cells := make([][]string, len(rows))
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	for i, r := range rows {
		cells[i] = make([]string, len(cols))
		for j := 0; j < len(cols); j++ {
			s := ""
			if j < len(r) && r[j] != nil {
				s = fmt.Sprintf("%v", r[j])
			}
			cells[i][j] = s
			if len(s) > widths[j] {
				widths[j] = len(s)
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(joinPadded(cols, widths))
	sb.WriteString("\n")
	var sep []string
	for j := range cols {
		sep = append(sep, strings.Repeat("-", widths[j]))
	}
	sb.WriteString(strings.Join(sep, "-") + "\n")
	for _, row := range cells {
		sb.WriteString(joinPadded(row, widths))
		sb.WriteString("\n")
	}
	return sb.String()
}

func joinPadded(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, s := range cells {
		if i < len(widths) && len(s) < widths[i] {
			s += strings.Repeat(" ", widths[i]-len(s))
		}
		parts[i] = s
	}
	return strings.Join(parts, " | ")
}

func repl(conn net.Conn) {
	fmt.Println("tsql — TSQL interactive terminal (\\q quit, \\dt tables)")
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("tsql> ")
		line, err := in.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println()
			}
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "\\q", "quit", "exit":
			return
		case "\\dt":
			listTables(conn)
			continue
		}
		msgs, err := sendQuery(conn, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsql: connection error: %v\n", err)
			return
		}
		printResults(os.Stdout, msgs)
	}
}

func listTables(conn net.Conn) {
	if err := protocol.WriteFrame(conn, protocol.Message{Type: "tables"}); err != nil {
		fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
		return
	}
	for {
		m, err := protocol.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
			return
		}
		if m.Type == "tables" {
			if len(m.Tables) == 0 {
				fmt.Println("(no tables)")
				return
			}
			for _, t := range m.Tables {
				fmt.Println(t)
			}
			return
		}
		if m.Type == "ready" {
			return
		}
	}
}
