package website

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"apibox.club/utils"
	"github.com/gorilla/websocket"
	gossh "golang.org/x/crypto/ssh"
)

var (
	aesKey string = "$hejGRT^$*#@#12o"
)

type ssh struct {
	user    string
	pwd     string
	addr    string
	client  *gossh.Client
	session *gossh.Session
}

func (s *ssh) Connect() (*ssh, error) {
	config := &gossh.ClientConfig{}
	config.SetDefaults()
	config.User = s.user
	config.Auth = []gossh.AuthMethod{gossh.Password(s.pwd)}
	client, err := gossh.Dial("tcp", s.addr, config)
	if nil != err {
		return nil, err
	}
	s.client = client
	return s, nil
}

func (s *ssh) Exec(cmd string) (string, error) {
	var buf bytes.Buffer
	session, err := s.client.NewSession()
	if nil != err {
		return "", err
	}
	session.Stdout = &buf
	session.Stderr = &buf
	err = session.Run(cmd)
	if err != nil {
		return "", err
	}
	defer session.Close()
	stdout := buf.String()
	apibox.Log_Debug("Stdout:", stdout)
	return stdout, nil
}

func chkSSHSrvAddr(ssh_addr, key string) (string, string, error) {

	if strings.Index(ssh_addr, "//") <= 0 {
		ssh_addr = "//" + ssh_addr
	}

	u, err := url.Parse(ssh_addr)
	if nil != err {
		return "", "", err
	}
	var new_url, new_host string
	if "" == u.Host {
		new_host = u.String()
	} else {
		new_host = u.Host
	}
	urls := strings.Split(new_host, ":")
	if len(urls) != 2 {
		new_url = new_host + ":22"
	} else {
		new_url = new_host
	}
	addr, err := net.ResolveTCPAddr("tcp4", new_url)
	if nil != err {
		return "", "", err
	}
	en_addr, err := apibox.AESEncode(addr.String(), key)
	if nil != err {
		return "", "", err
	}
	return addr.String(), en_addr, nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// 跨域处理，这里需要做一下安全防护。比如：请求白名单(这里只是简单的做了请求HOST白名单)
		cwl := Conf.Web.CorsWhiteList
		apibox.Log_Debug("Cors white list:", cwl)
		apibox.Log_Debug("Request Host:", cwl)
		for _, v := range strings.Split(cwl, ",") {
			if strings.EqualFold(strings.TrimSpace(v), r.Host) {
				return true
			}
		}
		return false
	},
}

type ptyRequestMsg struct {
	Term     string
	Columns  uint32
	Rows     uint32
	Width    uint32
	Height   uint32
	Modelist string
}

type jsonMsg struct {
	Data string `json:"data"`
}

func SSHWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	ctx := NewContext(w, r)
	ws, err := upgrader.Upgrade(w, r, nil)
	if nil != err {
		apibox.Log_Err("Upgrade WebScoket Error:", err)
		return
	}
	defer ws.Close()

	vm_info := ctx.GetFormValue("vm_info")
	cols := ctx.GetFormValue("cols")
	rows := ctx.GetFormValue("rows")

	apibox.Log_Debug("VM Info:", vm_info, "Cols:", cols, "Rows:", rows)

	de_vm_info, err := apibox.AESDecode(vm_info, aesKey)
	if nil != err {
		apibox.Log_Err("AESDecode:", err)
		return
	} else {
		de_vm_info_arr := strings.Split(de_vm_info, "\n")
		if len(de_vm_info_arr) == 3 {
			user_name := strings.TrimSpace(de_vm_info_arr[0])
			user_pwd := strings.TrimSpace(de_vm_info_arr[1])
			vm_addr := strings.TrimSpace(de_vm_info_arr[2])

			apibox.Log_Debug("VM Addr:", vm_addr)

			sh := &ssh{
				user: user_name,
				pwd:  user_pwd,
				addr: vm_addr,
			}
			sh, err = sh.Connect()
			if nil != err {
				apibox.Log_Err(err)
				return
			}

			ptyCols, err := apibox.StringUtils(cols).Uint32()
			if nil != err {
				apibox.Log_Err(err)
				return
			}
			ptyRows, err := apibox.StringUtils(rows).Uint32()
			if nil != err {
				apibox.Log_Err(err)
				return
			}

			channel, incomingRequests, err := sh.client.Conn.OpenChannel("session", nil)
			if err != nil {
				apibox.Log_Err(err)
				return
			}
			go func() {
				for req := range incomingRequests {
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}()
			modes := gossh.TerminalModes{
				gossh.ECHO:          1,
				gossh.TTY_OP_ISPEED: 14400,
				gossh.TTY_OP_OSPEED: 14400,
			}
			var modeList []byte
			for k, v := range modes {
				kv := struct {
					Key byte
					Val uint32
				}{k, v}
				modeList = append(modeList, gossh.Marshal(&kv)...)
			}
			modeList = append(modeList, 0)
			req := ptyRequestMsg{
				Term:     "xterm",
				Columns:  ptyCols,
				Rows:     ptyRows,
				Width:    ptyCols * 8,
				Height:   ptyRows * 8,
				Modelist: string(modeList),
			}
			ok, err := channel.SendRequest("pty-req", true, gossh.Marshal(&req))
			if !ok || err != nil {
				apibox.Log_Err(err)
				return
			}
			ok, err = channel.SendRequest("shell", true, nil)
			if !ok || err != nil {
				apibox.Log_Err(err)
				return
			}

			done := make(chan bool, 2)
			go func() {
				defer func() {
					done <- true
				}()

				for {
					m, p, err := ws.ReadMessage()
					if err != nil {
						apibox.Log_Warn(err.Error())
						return
					}

					if m == websocket.TextMessage {
						if _, err := channel.Write(p); nil != err {
							return
						}
					}
				}
			}()
			go func() {
				defer func() {
					done <- true
				}()
				rbuf := make([]byte, 1024)
				for {
					n, err := channel.Read(rbuf)

					if io.EOF == err {
						return
					}
					if err != nil {
						apibox.Log_Err(err.Error())
						return
					}
					if n > 0 {
						err = ws.WriteMessage(websocket.TextMessage, rbuf[:n])
						if err != nil {
							apibox.Log_Err(err)
							return
						}
					}
				}
			}()
			<-done
		} else {
			apibox.Log_Err("Unable to parse the data.")
			return
		}
	}
}

type Console struct {
}

type LoginPageData struct {
	VM_Name    string `json:"vm_name" xml:"vm_name"`
	VM_Addr    string `json:"vm_addr" xml:"vm_addr"`
	EN_VM_Name string `json:"en_vm_name" xml:"en_vm_name"`
	EN_VM_Addr string `json:"en_vm_addr" xml:"en_vm_addr"`
	Token      string `json:"token" xml:"token"`
}

func (c *Console) ConsoleLoginPage(w http.ResponseWriter, r *http.Request) {
	ctx := NewContext(w, r)
	vm_addr := ctx.GetFormValue("vm_addr")

	de_vm_addr, vm_addr_err := apibox.AESDecode(vm_addr, aesKey)
	if vm_addr == "" || nil != vm_addr_err {
		ctx.OutHtml("login", nil)
	} else {
		lpd := LoginPageData{
			VM_Addr:    de_vm_addr,
			EN_VM_Addr: vm_addr,
			Token:      apibox.StringUtils("sss").Base64Encode(),
		}
		ctx.OutHtml("console/console_login", lpd)
	}
}

type ConsoleMainPageData struct {
	Token    string `json:"token" xml:"token"`
	UserName string `json:"user_name" xml:"user_name"`
	UserPwd  string `json:"user_pwd" xml:"user_pwd"`
	VM_Name  string `json:"vm_name" xml:"vm_name"`
	VM_Addr  string `json:"vm_addr" xml:"vm_addr"`
	WS_Addr  string `json:"ws_addr" xml:"ws_addr"`
}

func (c *Console) ConsoleMainPage(w http.ResponseWriter, r *http.Request) {
	ctx := NewContext(w, r)

	vm_info := ctx.GetFormValue("vm_info")

	apibox.Log_Debug("VM Info:", vm_info)

	de_vm_info, err := apibox.AESDecode(vm_info, aesKey)
	if nil != err {
		apibox.Log_Err("AESDecode:", err)
		ctx.OutHtml("login", nil)
	} else {
		de_vm_info_arr := strings.Split(de_vm_info, "\n")
		if len(de_vm_info_arr) == 3 {
			user_name := strings.TrimSpace(de_vm_info_arr[0])
			user_pwd := strings.TrimSpace(de_vm_info_arr[1])
			vm_addr := strings.TrimSpace(de_vm_info_arr[2])

			cmpd := ConsoleMainPageData{
				UserName: user_name,
				UserPwd:  user_pwd,
				VM_Addr:  vm_addr,
			}
			wsAddr := r.Host + "/console/sshws/" + vm_info
			apibox.Log_Debug("WS Addr:", wsAddr)
			cmpd.WS_Addr = wsAddr
			ctx.OutHtml("console/console_main", cmpd)
		} else {
			ctx.OutHtml("login", nil)
		}
	}
}

func (c *Console) ConsoleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := NewContext(w, r)

	user_name := ctx.GetFormValue("user_name")
	user_pwd := ctx.GetFormValue("user_pwd")
	vm_addr := ctx.GetFormValue("vm_addr")

	var err error
	boo := true

	vm_addr_arr := strings.Split(vm_addr, ":")

	if len(vm_addr_arr) != 2 {
		boo = false
	}

	result := &Result{}
	if boo {
		sh := &ssh{
			user: user_name,
			pwd:  user_pwd,
			addr: vm_addr,
		}
		sh, err = sh.Connect()
		if nil != err {
			result.Ok = false
			result.Msg = "无法连接到远端主机，请确认远端主机已开机且保证口令的正确性。"
		} else {
			_, err := sh.Exec("true")
			if nil != err {
				result.Ok = false
				result.Msg = "用户无权限访问到远端主机，请联系系统管理员。"
			} else {
				ssh_info := make([]string, 0, 0)
				ssh_info = append(ssh_info, user_name)
				ssh_info = append(ssh_info, user_pwd)
				ssh_info = append(ssh_info, vm_addr)
				b64_ssh_info, err := apibox.AESEncode(strings.Join(ssh_info, "\n"), aesKey)
				if nil != err {
					apibox.Log_Err("AESEncode:", err)
					result.Ok = false
					result.Msg = "内部错误，请联系管理员（postmaster@apibox.club）。"
				} else {
					result.Ok = true
					result.Data = "/console/main/" + b64_ssh_info
				}
			}
		}
	} else {
		result.Ok = false
		result.Msg = "内部错误，请联系管理员（postmaster@apibox.club）。"
	}
	ctx.OutJson(result)
}

func (c *Console) ConsoleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := NewContext(w, r)
	ctx.OutHtml("login", nil)
}

func (c *Console) ChkSSHSrvAddr(w http.ResponseWriter, r *http.Request) {
	result := &Result{}
	ctx := NewContext(w, r)
	vm_addr := ctx.GetFormValue("vm_addr")
	if vm_addr == "" {
		result.Ok = false
		result.Msg = "Invalid host address."
	} else {
		sshd_addr, en_addr, err := chkSSHSrvAddr(vm_addr, aesKey)
		if nil != err {
			result.Ok = false
			result.Msg = "Unable to resolve host address."
		} else {
			chkMap := make(map[string]string)
			chkMap["sshd_addr"] = sshd_addr
			chkMap["en_addr"] = en_addr

			result.Ok = true
			result.Data = chkMap
		}
	}
	ctx.OutJson(result)
}

func init() {
	aesKey, _ = apibox.StringUtils("").UUID16()
	console := &Console{}
	Add_HandleFunc("get,post", "/", console.ConsoleLoginPage)
	Add_HandleFunc("get,post", "/console/chksshdaddr", console.ChkSSHSrvAddr)
	Add_HandleFunc("get,post", "/console/login/:vm_addr", console.ConsoleLoginPage)
	Add_HandleFunc("get,post", "/console/login", console.ConsoleLogin)
	Add_HandleFunc("get,post", "/console/logout", console.ConsoleLogout)
	Add_HandleFunc("get,post", "/console/main/:vm_info", console.ConsoleMainPage)
	Add_HandleFunc("get,post", "/console/sshws/:vm_info", SSHWebSocketHandler)
}
