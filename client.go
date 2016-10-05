package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 5 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

// Client represents a connected Koha intra UI client with RFID-capabilities.
type Client struct {
	state          RFIDState
	branch         string
	current        Message
	items          map[string]Message // Keep items around for retries, keyed by barcode TODO drop Message, store only Item
	failedAlarmOn  map[string]string  // map[Barcode]Tag
	failedAlarmOff map[string]string  // map[Barcode]Tag
	IP             string
	hub            *Hub
	conn           *websocket.Conn
	rfidconn       net.Conn
	rfid           *RFIDManager
	readBuf        []byte
	fromKoha       chan Message
	fromRFID       chan RFIDResp
	quit           chan bool
}

// Run the state-machine of the client
func (c *Client) Run(cfg Config) {
	go c.initRFID(cfg.RFIDPort)
	for {
		select {
		case msg := <-c.fromKoha:
			switch msg.Action {
			case "CHECKIN":
				c.state = RFIDCheckinWaitForBegOK
				c.rfid.Reset()
				c.branch = msg.Branch
				c.sendToRFID(RFIDReq{Cmd: cmdBeginScan})
			case "END":
				c.state = RFIDWaitForEndOK
			case "ITEM-INFO":
			case "WRITE":
			case "CHECKOUT":
			case "RETRY-ALARM-ON":
			case "RETRY-ALARM-OFF":
			}
		case resp := <-c.fromRFID:
			switch c.state {
			case RFIDCheckinWaitForBegOK:
				if !resp.OK {
					log.Printf("ERR: [%v] RFID failed to start scanning", c.IP)
					c.sendToKoha(Message{Action: "CONNECT", RFIDError: true})
					c.quit <- true
					break
				}
				c.state = RFIDCheckin
			case RFIDCheckin:
				var err error
				if !resp.OK {
					// Not OK on checkin means missing tags

					// Get item info from SIP, in order to have a title to display
					// Don't bother calling SIP if this is allready the current item
					if stripLeading10(resp.Barcode) != c.current.Item.Barcode {
						c.current, err = DoSIPCall(c.hub.config, sipPool, sipFormMsgItemStatus(resp.Barcode), itemStatusParse)
						if err != nil {
							log.Println("ERR [%s] SIP: %v", c.IP, err)
							c.sendToKoha(Message{Action: "CONNECT", SIPError: true, ErrorMessage: err.Error()})
							c.quit <- true
							break
						}
					}
					c.current.Action = "CHECKIN"
					c.items[stripLeading10(resp.Barcode)] = c.current
					c.sendToRFID(RFIDReq{Cmd: cmdAlarmLeave})
					c.state = RFIDWaitForCheckinAlarmLeave
					break
				} else {
					// Proceed with checkin transaciton
					c.current, err = DoSIPCall(c.hub.config, sipPool, sipFormMsgCheckin(c.branch, resp.Barcode), checkinParse)
					if err != nil {
						log.Println("ERR [%s] SIP call failed: %v", c.IP, err)
						c.sendToKoha(Message{Action: "CHECKIN", SIPError: true, ErrorMessage: err.Error()})
						// TODO send cmdAlarmLeave to RFID?
						break
					}
					if c.current.Item.Unknown || c.current.Item.TransactionFailed {
						c.sendToRFID(RFIDReq{Cmd: cmdAlarmLeave})
						c.state = RFIDWaitForCheckinAlarmLeave
					} else {
						c.items[stripLeading10(resp.Barcode)] = c.current
						c.failedAlarmOn[stripLeading10(resp.Barcode)] = resp.Tag // Store tag id for potential retry
						c.sendToRFID(RFIDReq{Cmd: cmdAlarmOn})
						c.state = RFIDWaitForCheckinAlarmOn
					}
				}
			}
		case <-c.quit:
			c.write(websocket.CloseMessage, []byte{})
			return
		}
	}
}

func (c *Client) initRFID(port string) {
	var err error
	c.rfidconn, err = net.Dial("tcp", net.JoinHostPort(c.IP, port))
	if err != nil {
		log.Printf("ERR [%s] RFID server tcp connect: %v", c.IP, err)
		c.sendToKoha(Message{Action: "CONNECT", RFIDError: true, ErrorMessage: err.Error()})
		c.quit <- true
		return
	}
	// Init the RFID-unit with version command
	var initError string
	req := c.rfid.GenRequest(RFIDReq{Cmd: cmdInitVersion})
	_, err = c.rfidconn.Write(req)
	if err != nil {
		initError = err.Error()
	}
	log.Printf("-> [%s] %q", c.IP, string(req))

	r := bufio.NewReader(c.rfidconn)
	n, err := r.Read(c.readBuf)
	if err != nil {
		initError = err.Error()
	}
	resp, err := c.rfid.ParseResponse(c.readBuf[:n])
	if err != nil {
		initError = err.Error()
	}
	log.Printf("<- [%s] %q", c.IP, string(c.readBuf[:n]))

	if initError == "" && !resp.OK {
		initError = "RFID-unit responded with NOK"
	}

	if initError != "" {
		log.Printf("ERR [%s] RFID initialization: %s", c.IP, initError)
		c.sendToKoha(Message{Action: "CONNECT", RFIDError: true, ErrorMessage: initError})
		c.quit <- true
		return
	}

	log.Printf("[%s] RIFD connected & initialized", c.IP)

	go c.readFromRFID(r)

	// Notify UI of success:
	c.sendToKoha(Message{Action: "CONNECT"})
}

func (c *Client) readFromKoha() {
	defer func() {
		c.hub.Disconnect(c)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, jsonMsg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		var msg Message
		if err := json.Unmarshal(jsonMsg, &msg); err != nil {
			log.Println("ERR [%s] unmarshal message: %v", c.IP, err)
			continue
		}
		c.fromKoha <- msg
	}
}

// write writes a message with the given message type and payload.
func (c *Client) write(mt int, payload []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteMessage(mt, payload)
}

func (c *Client) sendToKoha(msg Message) {
	c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	w, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return
	}
	b, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ERR sendToKoha json.Marshal(msg): %v", err)
		return
	}
	w.Write(b)

	if err := w.Close(); err != nil {
		return
	}
}

func (c *Client) readFromRFID(r *bufio.Reader) {
	for {
		b, err := r.ReadBytes('\r')
		if err != nil {
			log.Printf("[%v] RFID server tcp read failed: %v", c.IP, err)
			c.sendToKoha(Message{Action: "CONNECT", RFIDError: true, ErrorMessage: err.Error()})
			c.quit <- true
			break
		}
		log.Printf("<- [%v] %q", c.IP, string(b))

		resp, err := c.rfid.ParseResponse(b)
		if err != nil {
			log.Printf("ERR [%v] %v", c.IP, err)
			c.sendToKoha(Message{Action: "CONNECT", RFIDError: true, ErrorMessage: err.Error()})
			c.quit <- true // TODO really?
			break
		}
		c.fromRFID <- resp
	}
}

func (c *Client) sendToRFID(req RFIDReq) {
	b := c.rfid.GenRequest(req)
	_, err := c.rfidconn.Write(b)
	if err != nil {
		log.Printf("ERR [%v] %v", c.IP, err)
		c.sendToKoha(Message{Action: "CONNECT", RFIDError: true, ErrorMessage: err.Error()})
		c.quit <- true
		return
	}
	log.Printf("-> [%v] %q", c.IP, string(b))
}

func stripLeading10(barcode string) string {
	return strings.TrimPrefix(barcode, "10")
}
