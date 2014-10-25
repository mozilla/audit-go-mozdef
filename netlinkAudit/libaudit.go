package netlinkAudit

import (
	"bytes"
	"encoding/json"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var nextSeqNr uint32

type AuditStatus struct {
	Mask          uint32 /* Bit mask for valid entries */
	Enabled       uint32 /* 1 = enabled, 0 = disabled */
	Failure       uint32 /* Failure-to-log action */
	Pid           uint32 /* pid of auditd process */
	Rate_limit    uint32 /* messages rate limit (per second) */
	Backlog_limit uint32 /* waiting messages limit */
	Lost          uint32 /* messages lost */
	Backlog       uint32 /* messages waiting in queue */
}

type AuditRuleData struct {
	Flags       uint32 /* AUDIT_PER_{TASK,CALL}, AUDIT_PREPEND */
	Action      uint32 /* AUDIT_NEVER, AUDIT_POSSIBLE, AUDIT_ALWAYS */
	Field_count uint32
	Mask        [AUDIT_BITMASK_SIZE]uint32 /* syscall(s) affected */
	Fields      [AUDIT_MAX_FIELDS]uint32
	Values      [AUDIT_MAX_FIELDS]uint32
	Fieldflags  [AUDIT_MAX_FIELDS]uint32
	Buflen      uint32  /* total length of string fields */
	Buf         [0]byte //[0]string /* string fields buffer */

}
type NetlinkSocket struct {
	fd  int
	lsa syscall.SockaddrNetlink
}

type NetlinkAuditRequest struct {
	Header syscall.NlMsghdr
	Data   []byte
}

// for config
type CMap struct {
	Name  string
	Id  int
}

// for config
type Config struct {
	Xmap []CMap
}

var ParsedResult AuditStatus

func nativeEndian() binary.ByteOrder {
	var x uint32 = 0x01020304
	if *(*byte)(unsafe.Pointer(&x)) == 0x01 {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

//The recvfrom in go takes only a byte [] to put the data recieved from the kernel that removes the need
//for having a separate audit_reply Struct for recieving data from kernel.
func (rr *NetlinkAuditRequest) ToWireFormat() []byte {
	b := make([]byte, rr.Header.Len)
	*(*uint32)(unsafe.Pointer(&b[0:4][0])) = rr.Header.Len
	*(*uint16)(unsafe.Pointer(&b[4:6][0])) = rr.Header.Type
	*(*uint16)(unsafe.Pointer(&b[6:8][0])) = rr.Header.Flags
	*(*uint32)(unsafe.Pointer(&b[8:12][0])) = rr.Header.Seq
	*(*uint32)(unsafe.Pointer(&b[12:16][0])) = rr.Header.Pid
	b = append(b[:16], rr.Data[:]...) //Important b[:16]
	return b
}

func newNetlinkAuditRequest(proto /*seq,*/, family, sizeofData int) *NetlinkAuditRequest {
	rr := &NetlinkAuditRequest{}

	rr.Header.Len = uint32(syscall.NLMSG_HDRLEN + sizeofData)
	rr.Header.Type = uint16(proto)
	rr.Header.Flags = syscall.NLM_F_REQUEST | syscall.NLM_F_ACK
	rr.Header.Seq = atomic.AddUint32(&nextSeqNr, 1) //Autoincrementing Sequence
	return rr
	//	return rr.ToWireFormat()
}

// Round the length of a netlink message up to align it properly.
func nlmAlignOf(msglen int) int {
	return (msglen + syscall.NLMSG_ALIGNTO - 1) & ^(syscall.NLMSG_ALIGNTO - 1)
}

/*
 NLMSG_HDRLEN     ((int) NLMSG_ALIGN(sizeof(struct nlmsghdr)))
 NLMSG_LENGTH(len) ((len) + NLMSG_HDRLEN)
 NLMSG_SPACE(len) NLMSG_ALIGN(NLMSG_LENGTH(len))
 NLMSG_DATA(nlh)  ((void*)(((char*)nlh) + NLMSG_LENGTH(0)))
 NLMSG_NEXT(nlh,len)      ((len) -= NLMSG_ALIGN((nlh)->nlmsg_len), \
                                    (struct nlmsghdr*)(((char*)(nlh)) + NLMSG_ALIGN((nlh)->nlmsg_len)))
 NLMSG_OK(nlh,len) ((len) >= (int)sizeof(struct nlmsghdr) && \
                             (nlh)->nlmsg_len >= sizeof(struct nlmsghdr) && \
                             (nlh)->nlmsg_len <= (len))
*/

func ParseAuditNetlinkMessage(b []byte) ([]syscall.NetlinkMessage, error) {
	var msgs []syscall.NetlinkMessage
	//What is the reason for looping ?
	//	for len(b) >= syscall.NLMSG_HDRLEN {
	h, dbuf, dlen, err := netlinkMessageHeaderAndData(b)
	if err != nil {
		fmt.Println("Error in parsing")
		return nil, err
	}
	//fmt.Println("Get 3", h, dbuf, dlen, len(b))
	m := syscall.NetlinkMessage{Header: *h, Data: dbuf[:int(h.Len) /* -syscall.NLMSG_HDRLEN*/]}
	//Commented the subtraction. Leading to trimming of the output Data string
	msgs = append(msgs, m)
	b = b[dlen:]
	//	}
	//fmt.Println("Passed", msgs)
	return msgs, nil
}

func netlinkMessageHeaderAndData(b []byte) (*syscall.NlMsghdr, []byte, int, error) {

	h := (*syscall.NlMsghdr)(unsafe.Pointer(&b[0]))
	//fmt.Println("HEADER 1", b[0], b)
	if int(h.Len) < syscall.NLMSG_HDRLEN || int(h.Len) > len(b) {
		foo := int32(nativeEndian().Uint32(b[0:4]))
		fmt.Println("Headerlength with ", foo, b[0]) //!bug ! FIX THIS
		//IT happens only when message don't contain any useful message only dummy strings
		fmt.Println("Error due to....HDRLEN:", syscall.NLMSG_HDRLEN, " Header Length:", h.Len, " Length of BYTE Array:", len(b))
		return nil, nil, 0, syscall.EINVAL
	}
	//fmt.Println("OUT 2", b[0], b, syscall.NLMSG_HDRLEN)
	return h, b[syscall.NLMSG_HDRLEN:], nlmAlignOf(int(h.Len)), nil
}

// This function makes a conncetion with kernel space and is to be used for all further socket communication

func GetNetlinkSocket() (*NetlinkSocket, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_AUDIT) //connect to the socket of type RAW
	if err != nil {
		return nil, err
	}
	s := &NetlinkSocket{
		fd: fd,
	}
	s.lsa.Family = syscall.AF_NETLINK
	s.lsa.Groups = 0
	s.lsa.Pid = 0 //Kernel space pid is always set to be 0

	if err := syscall.Bind(fd, &s.lsa); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return s, nil
}

//To end the socket conncetion
func (s *NetlinkSocket) Close() {
	syscall.Close(s.fd)
}

func (s *NetlinkSocket) Send(request *NetlinkAuditRequest) error {
	if err := syscall.Sendto(s.fd, request.ToWireFormat(), 0, &s.lsa); err != nil {
		return err
	}
	return nil
}

func (s *NetlinkSocket) Receive(bytesize int, block int) ([]syscall.NetlinkMessage, error) {
	rb := make([]byte, bytesize)
	nr, _, err := syscall.Recvfrom(s.fd, rb, 0|block)
	//nr, _, err := syscall.Recvfrom(s, rb, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)

	if err != nil {
		return nil, err
	}
	if nr < syscall.NLMSG_HDRLEN {
		return nil, syscall.EINVAL
	}
	rb = rb[:nr]
	return ParseAuditNetlinkMessage(rb) //Or syscall.ParseNetlinkMessage(rb)
}

//Should we remove AuditSend ?
func AuditSend(s *NetlinkSocket, proto int, data []byte, sizedata /*,seq */ int) error {

	wb := newNetlinkAuditRequest(proto, syscall.AF_NETLINK, sizedata)
	wb.Data = append(wb.Data[:], data[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}
	return nil
}

//should it be changed to HandleAck ?
func AuditGetReply(s *NetlinkSocket, bytesize, block int, seq uint32) error {
done:
	for {
		msgs, err := s.Receive(bytesize, block) //ParseAuditNetlinkMessage(rb)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			lsa, err := syscall.Getsockname(s.fd)
			if err != nil {
				return err
			}
			switch v := lsa.(type) {
			case *syscall.SockaddrNetlink:

				if m.Header.Seq != seq {
					return fmt.Errorf("Wrong Seq nr %d, expected %d", m.Header.Seq, seq)
				}
				if m.Header.Pid != v.Pid {
					return fmt.Errorf("Wrong pid %d, expected %d", m.Header.Pid, v.Pid)
				}
			default:
				return syscall.EINVAL
			}

			if m.Header.Type == syscall.NLMSG_DONE {
				break done
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				error := int32(nativeEndian().Uint32(m.Data[0:4]))
				if error == 0 {
					fmt.Println("ACK")
					break done
				} else {
					fmt.Println("NLMSG_ERROR")
				}
				break done
			}
			if m.Header.Type == AUDIT_GET {
				fmt.Println("AUDIT_GET")
				//				break done
			}
			if m.Header.Type == AUDIT_FIRST_USER_MSG {
				fmt.Println("AUDIT_FIRST_USER_MS")
				//break done
			}
			if m.Header.Type == AUDIT_LIST_RULES {
				fmt.Println("AUDIT_LIST_RULES")
				//break done
			}
			if m.Header.Type == AUDIT_FIRST_USER_MSG {
				fmt.Println("AUDIT_FIRST_USER_MSG")
				//break done
			}
		}
	}
	return nil
}

func AuditSetEnabled(s *NetlinkSocket /*, seq int*/) error {
	var status AuditStatus
	status.Enabled = 1
	status.Mask = AUDIT_STATUS_ENABLED
	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), status)
	if err != nil {
		fmt.Println("binary.Write failed:", err)
		return err
	}

	//	err = AuditSend(s, AUDIT_SET, buff.Bytes(), int(unsafe.Sizeof(status)))
	/*
		if err != nil {
			return err
		}*/

	wb := newNetlinkAuditRequest(AUDIT_SET, syscall.AF_NETLINK, int(unsafe.Sizeof(status)))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}
	// Receiving IN JUST ONE TRY
	err = AuditGetReply(s, syscall.Getpagesize(), 0, wb.Header.Seq)
	if err != nil {
		return err
	}
	return nil
}

func AuditIsEnabled(s *NetlinkSocket /*seq int*/) error {
	wb := newNetlinkAuditRequest(AUDIT_GET, syscall.AF_NETLINK, 0)

	if err := s.Send(wb); err != nil {
		return err
	}

done:
	for {
		//Make the rb byte bigger because of large messages from Kernel doesn't fit in 4096
		msgs, err := s.Receive(MAX_AUDIT_MESSAGE_LENGTH, syscall.MSG_DONTWAIT) //ParseAuditNetlinkMessage(rb)
		if err != nil {
			return err
		}

		for _, m := range msgs {
			lsa, er := syscall.Getsockname(s.fd)
			if er != nil {
				return nil
			}
			switch v := lsa.(type) {
			case *syscall.SockaddrNetlink:

				if m.Header.Seq != uint32(wb.Header.Seq) || m.Header.Pid != v.Pid {
					return syscall.EINVAL
				}
			default:
				return syscall.EINVAL
			}
			if m.Header.Type == syscall.NLMSG_DONE {
				fmt.Println("Done")
				break done

			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				fmt.Println("NLMSG_ERROR\n")
			}
			if m.Header.Type == AUDIT_GET {
				//Conversion of the data part written to AuditStatus struct
				//Nil error : successfuly parsed
				b := m.Data[:]
				buf := bytes.NewBuffer(b)
				var dumm AuditStatus
				err = binary.Read(buf, nativeEndian(), &dumm)
				ParsedResult = dumm
				fmt.Println("ENABLED")
				break done
			}

		}

	}
	return nil

}
func AuditSetPid(s *NetlinkSocket, pid uint32 /*,Wait mode WAIT_YES | WAIT_NO */) error {
	var status AuditStatus
	status.Mask = AUDIT_STATUS_PID
	status.Pid = pid
	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), status)
	if err != nil {
		fmt.Println("binary.Write failed:", err)
		return err
	}

	/*	err = AuditSend(s, AUDIT_SET, buff.Bytes(), int(unsafe.Sizeof(status)))
		if err != nil {
			return err
		}
	*/
	wb := newNetlinkAuditRequest(AUDIT_SET, syscall.AF_NETLINK, int(unsafe.Sizeof(status)))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}

	err = AuditGetReply(s, syscall.Getpagesize(), 0, wb.Header.Seq)
	if err != nil {
		return err
	}
	//Polling in GO Is it needed ?

	return nil

}
func auditWord(nr int) uint32 {
	audit_word := (uint32)((nr) / 32)
	return (uint32)(audit_word)
}

func auditBit(nr int) uint32 {
	audit_bit := 1 << ((uint32)(nr) - auditWord(nr)*32)
	return (uint32)(audit_bit)
}

func AuditRuleSyscallData(rule *AuditRuleData, scall int) error {
	word := auditWord(scall)
	bit := auditBit(scall)

	if word >= AUDIT_BITMASK_SIZE-1 {
		fmt.Println("Some error occured")
	}
	rule.Mask[word] |= bit
	return nil
}

func AuditAddRuleData(s *NetlinkSocket, rule *AuditRuleData, flags int, action int) error {

	if flags == AUDIT_FILTER_ENTRY {
		fmt.Println("Use of entry filter is deprecated")
		return nil
	}

	rule.Flags = uint32(flags)
	rule.Action = uint32(action)

	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), *rule)
	if err != nil {
		fmt.Println("binary.Write failed:", err)
		return err
	}
	//	err = AuditSend(s, AUDIT_ADD_RULE, buff.Bytes(), int(buff.Len())+int(rule.Buflen))
	wb := newNetlinkAuditRequest(AUDIT_ADD_RULE, syscall.AF_NETLINK, int(buff.Len())+int(rule.Buflen))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}

	if err != nil {
		fmt.Println("Error sending add rule data request ()")
		return err
	}
	return nil
}

//TODO: Add a DeleteAllRules Function
//A very gibberish hack right now , Need to work on the design of this package.
func isDone(msgchan chan<- syscall.NetlinkMessage, errchan chan<- error, done <-chan bool) bool {
	var d bool
	select {
	case d = <-done:
		close(msgchan)
		close(errchan)
	default:
	}
	return d
}

func GetreplyWithoutSync(s *NetlinkSocket) {
	for {
		rb := make([]byte, MAX_AUDIT_MESSAGE_LENGTH)
		nr, _, err := syscall.Recvfrom(s.fd, rb, 0 /*Do not use syscall.MSG_DONTWAIT*/)
		if err != nil {
			fmt.Println("Error While Recieving !!")
			continue
		}
		if nr < syscall.NLMSG_HDRLEN {
			fmt.Println("Message Too Short!!")
			continue
		}

		rb = rb[:nr]
		msgs, err := ParseAuditNetlinkMessage(rb) //Or syscall.ParseNetlinkMessage(rb)

		if err != nil {
			fmt.Println("Not Parsed Successfuly !!")
			continue
		}
		for _, m := range msgs {
			/*
				Not needed while receiving from kernel ?
							lsa, err := syscall.Getsockname(s.fd)
							if err != nil {
								errchan <- err
								continue
								//return err
							}
								switch v := lsa.(type) {
								case *syscall.SockaddrNetlink:

									if m.Header.Seq != uint32(seq) || m.Header.Pid != v.Pid {
										fmt.Println("foo", seq, m.Header.Seq)
										errchan <- syscall.EINVAL
										//return syscall.EINVAL
									}
								default:
									errchan <- syscall.EINVAL
									//return syscall.EINVAL

								}
			*/
			//Decide on various message Types
			if m.Header.Type == syscall.NLMSG_DONE {
				fmt.Println("Done")
			} else if m.Header.Type == syscall.NLMSG_ERROR {
				err := int32(nativeEndian().Uint32(m.Data[0:4]))
				if err == 0 {
					fmt.Println("Ack") //Acknowledgement from kernel
				}
				fmt.Println("NLMSG_ERROR")

			} else if m.Header.Type == AUDIT_GET {
				fmt.Println("AUDIT_GET")

			} else if m.Header.Type == AUDIT_FIRST_USER_MSG {
				fmt.Println("AUDIT_FIRST_USER_MSG")

			} else if m.Header.Type == AUDIT_SYSCALL {
				fmt.Println("Syscall Event")
				fmt.Println(string(m.Data[:]))

			} else if m.Header.Type == AUDIT_CWD {
				fmt.Println("CWD Event")
				fmt.Println(string(m.Data[:]))

			} else if m.Header.Type == AUDIT_PATH {
				fmt.Println("Path Event")
				fmt.Println(string(m.Data[:]))

			} else if m.Header.Type == AUDIT_EOE {
				fmt.Println("Event Ends ", string(m.Data[:]))
			} else if m.Header.Type == AUDIT_CONFIG_CHANGE {
				fmt.Println("Config Change ", string(m.Data[:]))
			} else {
				fmt.Println("UNKnown: ", m.Header.Type)

			}

		}

	}

}

func Getreply(s *NetlinkSocket, msgchan chan<- syscall.NetlinkMessage, errchan chan<- error, done <-chan bool) {

	//	rb := make([]byte, syscall.Getpagesize())
	for {
		rb := make([]byte, MAX_AUDIT_MESSAGE_LENGTH)
		nr, _, err := syscall.Recvfrom(s.fd, rb, 0 /*Do not use syscall.MSG_DONTWAIT*/)
		/*
			if isDone(msgchan, errchan, done) {
				return
			}
		*/
		if err != nil {
			//errchan <- err
			continue
		}
		if nr < syscall.NLMSG_HDRLEN {
			//errchan <- syscall.EINVAL
			continue
		}

		rb = rb[:nr]
		msgs, err := ParseAuditNetlinkMessage(rb) //Or syscall.ParseNetlinkMessage(rb)

		if err != nil {
			//errchan <- err
			continue
		}
		for _, m := range msgs {
			/*
				Not needed while receiving from kernel ?
							lsa, err := syscall.Getsockname(s.fd)
							if err != nil {
								errchan <- err
								continue
								//return err
							}
								switch v := lsa.(type) {
								case *syscall.SockaddrNetlink:

									if m.Header.Seq != uint32(seq) || m.Header.Pid != v.Pid {
										fmt.Println("foo", seq, m.Header.Seq)
										errchan <- syscall.EINVAL
										//return syscall.EINVAL
									}
								default:
									errchan <- syscall.EINVAL
									//return syscall.EINVAL

								}
			*/
			//Decide on various message Types
			if m.Header.Type == syscall.NLMSG_DONE {
				fmt.Println("Done")
				//msgchan <- m
				//continue
			} else if m.Header.Type == syscall.NLMSG_ERROR {
				err := int32(nativeEndian().Uint32(m.Data[0:4]))
				if err == 0 {
					fmt.Println("Ack") //Acknowledgement from kernel
					//continue
				}

				fmt.Println("NLMSG_ERROR")
				//msgchan <- m
				//continue
			} else if m.Header.Type == AUDIT_GET {
				fmt.Println("AUDIT_GET")
				//msgchan <- m
				//continue
			} else if m.Header.Type == AUDIT_FIRST_USER_MSG {
				fmt.Println("AUDIT_FIRST_USER_MSG")
				//msgchan <- m
				//continue
			} else if m.Header.Type == AUDIT_SYSCALL {
				fmt.Println("Syscall Event")
				fmt.Println(string(m.Data[:]))
				//msgchan <- m
			} else if m.Header.Type == AUDIT_CWD {
				fmt.Println("CWD Event")
				fmt.Println(string(m.Data[:]))
				//msgchan <- m
			} else if m.Header.Type == AUDIT_PATH {
				fmt.Println("Path Event")
				fmt.Println(string(m.Data[:]))
				//msgchan <- m
			} else if m.Header.Type == AUDIT_EOE {
				fmt.Println("Event Ends ", string(m.Data[:]))
			} else {
				fmt.Println("UNKnown: ", m.Header.Type)
				//msgchan <- m
			}
		}
	}
}

//Delete Rule Data Function
func AuditDeleteRuleData(s *NetlinkSocket, rule *AuditRuleData, flags int, action int) error{
	//var rc int;
	var sizePurpose AuditRuleData
	if (flags == AUDIT_FILTER_ENTRY) {
		fmt.Println("Error in delete")
		return nil;
	}
	rule.Flags = uint32(flags)
	rule.Action = uint32(action)

	buff := new(bytes.Buffer)
	err := binary.Write(buff, nativeEndian(), *rule)
	if err != nil {
		fmt.Println("binary.Write failed:", err)
		return err
	}
	wb := newNetlinkAuditRequest(AUDIT_DEL_RULE, syscall.AF_NETLINK, int(unsafe.Sizeof(sizePurpose)) + int(rule.Buflen))
	wb.Data = append(wb.Data[:], buff.Bytes()[:]...)
	if err := s.Send(wb); err != nil {
		return err
	}
	return nil;
}

// function that sets each rule after reading configuration file
func SetRules(s *NetlinkSocket) {
	// TODO: clean the code, add cases

	// Load all rules
	content, err := ioutil.ReadFile("netlinkAudit/audit.rules.json")
	if err!=nil{
        fmt.Print("Error:",err)
	}

	var rules interface{}
	err = json.Unmarshal(content, &rules)

	m := rules.(map[string]interface{})
	for k, v := range m {
    	switch k {
    		case "syscall_rule":

    			vi := v.([]interface{})
	    		for sruleNo := range vi {
	    			srule := vi[sruleNo].(map[string]interface{})
    				// Load x86 map
    				content2, err := ioutil.ReadFile("netlinkAudit/audit_x86.json")
					if err!=nil{
			    	    fmt.Print("Error:",err)
					}
    
					var conf Config
					err = json.Unmarshal([]byte(content2), &conf)
					if err != nil {
						fmt.Print("Error:", err)
					}
					for l := range conf.Xmap {
						if conf.Xmap[l].Name == srule["name"] {
							// set rules 
							fmt.Println("setting syscall rule", conf.Xmap[l].Name)
							var foo AuditRuleData
							AuditRuleSyscallData(&foo,  conf.Xmap[l].Id)
							foo.Fields[foo.Field_count] = AUDIT_ARCH
							foo.Fieldflags[foo.Field_count] = AUDIT_EQUAL
							foo.Values[foo.Field_count] = AUDIT_ARCH_X86_64
							foo.Field_count++
							AuditAddRuleData(s, &foo, AUDIT_FILTER_EXIT, AUDIT_ALWAYS)
						}
					}
				    //default:
				    //    fmt.Println(k, "is not yet supported")
		    }
	}
}

/*
var fieldStrings []byte
fieldStrings = {"a", "a1", "a2", "a3", "arch", "auid", "devmajor", "devminor", "dir", "egid", "euid", "exit", "field_compare", "filetype", "fsgid", "fsuid", "gid", "inode", "key", "loginuid", "msgtype", "obj_gid", "obj_lev_high", "obj_lev_low", "obj_role", "obj_type", "obj_uid", "obj_user",  "path",  "perm",  "pers",  "pid",  "ppid",  "sgid",  "subj_clr",  "subj_role",  "subj_sen",  "subj_type",  "subj_user",  "success",  "suid",  "uid" }

var fieldS2i_s []uint = {0,3,6,9,12,17,22,31,40,44,49,54,59,73,82,88,94,98,104,108,117,125,133,146,158,167,176,184,193,198,203,208,212,217,222,231,241,250,260,270,278,283}

var fieldS2i_i []int{200,201,202,203,11,9,100,101,107,6,2,103,111,108,8,4,5,102,210,9,12,110,23,22,20,21,109,19,105,106,10,0,18,7,17,14,16,15,13,104,3,1}

func S2i(strings *[]byte, s_table *uint, i_table *int, int n, s *[]byte,value *int) bool{
	        var left, right int
	
	        left = 0
	        right = n - 1
	        while left <= right {
	                var mid int
	                var r int
	
	                mid = (left + right) / 2
	                r =  int(s) - int(s_table[mid])
	                if (r == 0) {
	                        &value = i_table[mid]
	                        return true
	                }
	                if (r < 0)
	                        right = mid - 1
	                else
	                        left = mid + 1
	        }
	        return false
}


func int FlagS2i(s *[]byte, value *int) bool{
	var len, i int
	i = 0
	c []byte
	len = unsafe.Sizeof(s);
	copy := make([]byte, len+1)
	//char copy[len + 1];	
	for i < len {
		c = &s[i]
		if unicode.IsUpper(c){
			copy[i] = c - 'A' + 'a'
		}else{
			copy[i] = c
		}
		i = i + 1
	}
	copy[i] = 0;
	return S2i(fieldStrings, fieldS2i_s, fieldS2i_i, 42, copy, value);
}

func AuditNameToField(const char *field) bool{
//#ifndef NO_TABLES
	//var res bool
	
	if FlagS2i(field, res) != false
	    return true;
//#endif	    
	return false;
}

/* Form of main function
package main

import (
	"fmt"
	"github.com/..../netlinkAudit"
)
func main() {
	s, err := netlinkAudit.GetNetlinkSocket()
	if err != nil {
		fmt.Println(err)
	}
	defer s.Close()

	netlinkAudit.AuditSetEnabled(s, 1)
	err = netlinkAudit.AuditIsEnabled(s, 2)
	fmt.Println("parsedResult")
	fmt.Println(netlinkAudit.ParsedResult)
	if err == nil {
		fmt.Println("Horrah")
	}

}

*/