package sftp

import (
	"io"
	"os"
	"path"
	"syscall"
)

// Valid Method values:
// Get, Put, SetStat, Rename, Rmdir, Mkdir, Symlink, List, Stat, Readlink
type Request struct {
	Method   string
	Filepath string
	Pflags   uint32
	Attrs    []byte // convert to sub-struct
	Target   string // for renames and sym-links
	pktChan  chan packet
	cur_pkt  packet
	svr      *RequestServer
}

func newRequest(path string, svr *RequestServer) *Request {
	request := &Request{Filepath: path, svr: svr}
	go request.requestWorker()
	return request
}

func (r *Request) sendError(err error) error {
	return r.svr.sendError(r.cur_pkt, err)
}

func (r *Request) close() { close(r.pktChan) }

func (r *Request) requestWorker() error {
	for p := range r.pktChan {
		r.populate(p)
		r.cur_pkt = p
		handlers := r.svr.Handlers
		switch r.Method {
		case "Get":
			pkt := p.(*sshFxpReadPacket)
			return fileget(handlers.FileGet, r, pkt)
		case "Put":
			pkt := p.(*sshFxpWritePacket)
			return fileput(handlers.FilePut, r, pkt)
		case "SetStat", "Rename", "Rmdir", "Mkdir", "Symlink":
			return filecmd(handlers.FileCmd, r)
		case "List", "Stat", "Readlink":
			pkt := p.(packet)
			return fileinfo(handlers.FileInfo, r, pkt)
		case "Open": // no-op
		}
	}
	return nil
}

func fileget(h FileReader, r *Request, pkt *sshFxpReadPacket) error {
	reader, err := h.Fileread(r)
	if err != nil { return r.sendError(syscall.EBADF) }
	data := make([]byte, clamp(pkt.Len, maxTxPacket))
	n, err := reader.Read(data)
	if err != nil && (err != io.EOF || n == 0) {
		return r.sendError(err)
	}
	return r.svr.sendPacket(sshFxpDataPacket{
		ID:     pkt.ID,
		Length: uint32(n),
		Data:   data[:n],
	})
}
func fileput(h FileWriter, r *Request, pkt *sshFxpWritePacket) error {
	writer, err := h.Filewrite(r)
	if err != nil { return r.sendError(syscall.EBADF) }
	_, err = writer.Write(pkt.Data)
	return r.sendError(err)
}
func filecmd(h FileCmder, r *Request) error {
	err := h.Filecmd(r)
	return r.sendError(err)
}
func fileinfo(h FileInfoer, r *Request, pkt packet) error {
	finfo, err := h.Fileinfo(r)
	if err != nil { return r.sendError(err) }

	switch r.Method {
	case "List":
		dirname := path.Base(r.Filepath)
		ret := sshFxpNamePacket{ID: pkt.id()}
		for _, fi := range finfo {
			ret.NameAttrs = append(ret.NameAttrs, sshFxpNameAttr{
				Name:     fi.Name(),
				LongName: runLs(dirname, fi),
				Attrs:    []interface{}{fi},
			})
		}
	case "Stat":
		if len(finfo) == 0 {
			err = &os.PathError{"stat", r.Filepath, syscall.ENOENT}
			return r.sendError(err)
		}
		return r.svr.sendPacket(sshFxpStatResponse{
			ID:   pkt.id(),
			info: finfo[0],
		})
	case "Readlink":
		if len(finfo) == 0 {
			err = &os.PathError{"readlink", r.Filepath, syscall.ENOENT}
			return r.sendError(err)
		}
		return r.svr.sendPacket(sshFxpNamePacket{
			ID: pkt.id(),
			NameAttrs: []sshFxpNameAttr{{
				Name:     finfo[0].Name(),
				LongName: finfo[0].Name(),
				Attrs:    emptyFileStat,
			}},
		})
	}
	return r.sendError(err)
}

func (r *Request) populate(p interface{}) {
	// r.Filepath set in newRequest()
	switch p := p.(type) {
	case *sshFxpSetstatPacket:
		r.Method = "Setstat"
		r.Pflags = p.Flags
		r.Attrs = p.Attrs.([]byte)
	case *sshFxpFsetstatPacket:
		r.Method = "Setstat"
		r.Pflags = p.Flags
		r.Attrs = p.Attrs.([]byte)
	case *sshFxpRenamePacket:
		r.Method = "Rename"
		r.Target = p.Newpath
	case *sshFxpSymlinkPacket:
		r.Method = "Symlink"
		r.Target = p.Linkpath
	// below here method and path are all the data
	case *sshFxpReaddirPacket:
		r.Method = "List"
	case *sshFxpStatPacket, *sshFxpLstatPacket, *sshFxpFstatPacket,
		*sshFxpRealpathPacket, *sshFxpRemovePacket:
		r.Method = "Stat"
	case *sshFxpRmdirPacket:
		r.Method = "Rmdir"
	case *sshFxpReadlinkPacket:
		r.Method = "Readlink"
	case *sshFxpOpenPacket:
		r.Method = "Open"
	// special cases
	case *sshFxpReadPacket:
		r.Method = "Get"
		// data processed elsewhere
	case *sshFxpWritePacket:
		r.Method = "Put"
		// data processed elsewhere
	case *sshFxpMkdirPacket:
		r.Method = "Mkdir"
		//r.Attrs are ignored in ./packet.go
	}
}
