package sftpd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/eikenb/pipeat"
	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/drakkan/sftpgo/common"
	"github.com/drakkan/sftpgo/dataprovider"
	"github.com/drakkan/sftpgo/utils"
	"github.com/drakkan/sftpgo/vfs"
)

const osWindows = "windows"

type MockChannel struct {
	Buffer        *bytes.Buffer
	StdErrBuffer  *bytes.Buffer
	ReadError     error
	WriteError    error
	ShortWriteErr bool
}

func (c *MockChannel) Read(data []byte) (int, error) {
	if c.ReadError != nil {
		return 0, c.ReadError
	}
	return c.Buffer.Read(data)
}

func (c *MockChannel) Write(data []byte) (int, error) {
	if c.WriteError != nil {
		return 0, c.WriteError
	}
	if c.ShortWriteErr {
		return 0, nil
	}
	return c.Buffer.Write(data)
}

func (c *MockChannel) Close() error {
	return nil
}

func (c *MockChannel) CloseWrite() error {
	return nil
}

func (c *MockChannel) SendRequest(name string, wantReply bool, payload []byte) (bool, error) {
	return true, nil
}

func (c *MockChannel) Stderr() io.ReadWriter {
	return c.StdErrBuffer
}

// MockOsFs mockable OsFs
type MockOsFs struct {
	vfs.Fs
	err                     error
	statErr                 error
	isAtomicUploadSupported bool
}

// Name returns the name for the Fs implementation
func (fs MockOsFs) Name() string {
	return "mockOsFs"
}

// IsUploadResumeSupported returns true if upload resume is supported
func (MockOsFs) IsUploadResumeSupported() bool {
	return false
}

// IsAtomicUploadSupported returns true if atomic upload is supported
func (fs MockOsFs) IsAtomicUploadSupported() bool {
	return fs.isAtomicUploadSupported
}

// Stat returns a FileInfo describing the named file
func (fs MockOsFs) Stat(name string) (os.FileInfo, error) {
	if fs.statErr != nil {
		return nil, fs.statErr
	}
	return os.Stat(name)
}

// Lstat returns a FileInfo describing the named file
func (fs MockOsFs) Lstat(name string) (os.FileInfo, error) {
	if fs.statErr != nil {
		return nil, fs.statErr
	}
	return os.Lstat(name)
}

// Remove removes the named file or (empty) directory.
func (fs MockOsFs) Remove(name string, isDir bool) error {
	if fs.err != nil {
		return fs.err
	}
	return os.Remove(name)
}

// Rename renames (moves) source to target
func (fs MockOsFs) Rename(source, target string) error {
	if fs.err != nil {
		return fs.err
	}
	return os.Rename(source, target)
}

func newMockOsFs(err, statErr error, atomicUpload bool, connectionID, rootDir string) vfs.Fs {
	return &MockOsFs{
		Fs:                      vfs.NewOsFs(connectionID, rootDir, nil),
		err:                     err,
		statErr:                 statErr,
		isAtomicUploadSupported: atomicUpload,
	}
}

func TestRemoveNonexistentQuotaScan(t *testing.T) {
	assert.False(t, common.QuotaScans.RemoveUserQuotaScan("username"))
}

func TestGetOSOpenFlags(t *testing.T) {
	var flags sftp.FileOpenFlags
	flags.Write = true
	flags.Excl = true
	osFlags := getOSOpenFlags(flags)
	assert.NotEqual(t, 0, osFlags&os.O_WRONLY)
	assert.NotEqual(t, 0, osFlags&os.O_EXCL)

	flags.Append = true
	// append flag should be ignored to allow resume
	assert.NotEqual(t, 0, osFlags&os.O_WRONLY)
	assert.NotEqual(t, 0, osFlags&os.O_EXCL)
}

func TestUploadResumeInvalidOffset(t *testing.T) {
	testfile := "testfile" //nolint:goconst
	file, err := os.Create(testfile)
	assert.NoError(t, err)
	user := dataprovider.User{
		Username: "testuser",
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	conn := common.NewBaseConnection("", common.ProtocolSFTP, user, fs)
	baseTransfer := common.NewBaseTransfer(file, conn, nil, file.Name(), testfile, common.TransferUpload, 10, 0, 0, false, fs)
	transfer := newTransfer(baseTransfer, nil, nil, nil)
	_, err = transfer.WriteAt([]byte("test"), 0)
	assert.Error(t, err, "upload with invalid offset must fail")
	if assert.Error(t, transfer.ErrTransfer) {
		assert.EqualError(t, err, transfer.ErrTransfer.Error())
		assert.Contains(t, transfer.ErrTransfer.Error(), "Invalid write offset")
	}

	err = transfer.Close()
	if assert.Error(t, err) {
		assert.EqualError(t, err, sftp.ErrSSHFxFailure.Error())
	}

	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestReadWriteErrors(t *testing.T) {
	testfile := "testfile"
	file, err := os.Create(testfile)
	assert.NoError(t, err)

	user := dataprovider.User{
		Username: "testuser",
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	conn := common.NewBaseConnection("", common.ProtocolSFTP, user, fs)
	baseTransfer := common.NewBaseTransfer(file, conn, nil, file.Name(), testfile, common.TransferDownload, 0, 0, 0, false, fs)
	transfer := newTransfer(baseTransfer, nil, nil, nil)
	err = file.Close()
	assert.NoError(t, err)
	_, err = transfer.WriteAt([]byte("test"), 0)
	assert.Error(t, err, "writing to closed file must fail")
	buf := make([]byte, 32768)
	_, err = transfer.ReadAt(buf, 0)
	assert.Error(t, err, "reading from a closed file must fail")
	err = transfer.Close()
	assert.Error(t, err)

	r, _, err := pipeat.Pipe()
	assert.NoError(t, err)
	baseTransfer = common.NewBaseTransfer(nil, conn, nil, file.Name(), testfile, common.TransferDownload, 0, 0, 0, false, fs)
	transfer = newTransfer(baseTransfer, nil, r, nil)
	err = transfer.Close()
	assert.NoError(t, err)
	_, err = transfer.ReadAt(buf, 0)
	assert.Error(t, err, "reading from a closed pipe must fail")

	r, w, err := pipeat.Pipe()
	assert.NoError(t, err)
	pipeWriter := vfs.NewPipeWriter(w)
	baseTransfer = common.NewBaseTransfer(nil, conn, nil, file.Name(), testfile, common.TransferDownload, 0, 0, 0, false, fs)
	transfer = newTransfer(baseTransfer, pipeWriter, nil, nil)

	err = r.Close()
	assert.NoError(t, err)
	errFake := fmt.Errorf("fake upload error")
	go func() {
		time.Sleep(100 * time.Millisecond)
		pipeWriter.Done(errFake)
	}()
	err = transfer.closeIO()
	assert.EqualError(t, err, errFake.Error())
	_, err = transfer.WriteAt([]byte("test"), 0)
	assert.Error(t, err, "writing to closed pipe must fail")
	err = transfer.BaseTransfer.Close()
	assert.EqualError(t, err, errFake.Error())

	err = os.Remove(testfile)
	assert.NoError(t, err)
	assert.Len(t, conn.GetTransfers(), 0)
}

func TestUnsupportedListOP(t *testing.T) {
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	conn := common.NewBaseConnection("", common.ProtocolSFTP, dataprovider.User{}, fs)
	sftpConn := Connection{
		BaseConnection: conn,
	}
	request := sftp.NewRequest("Unsupported", "")
	_, err := sftpConn.Filelist(request)
	assert.EqualError(t, err, sftp.ErrSSHFxOpUnsupported.Error())
}

func TestTransferCancelFn(t *testing.T) {
	testfile := "testfile"
	file, err := os.Create(testfile)
	assert.NoError(t, err)
	isCancelled := false
	cancelFn := func() {
		isCancelled = true
	}
	user := dataprovider.User{
		Username: "testuser",
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	conn := common.NewBaseConnection("", common.ProtocolSFTP, user, fs)
	baseTransfer := common.NewBaseTransfer(file, conn, cancelFn, file.Name(), testfile, common.TransferDownload, 0, 0, 0, false, fs)
	transfer := newTransfer(baseTransfer, nil, nil, nil)

	errFake := errors.New("fake error, this will trigger cancelFn")
	transfer.TransferError(errFake)
	err = transfer.Close()
	if assert.Error(t, err) {
		assert.EqualError(t, err, sftp.ErrSSHFxFailure.Error())
	}
	if assert.Error(t, transfer.ErrTransfer) {
		assert.EqualError(t, transfer.ErrTransfer, errFake.Error())
	}
	assert.True(t, isCancelled, "cancelFn not called!")

	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestMockFsErrors(t *testing.T) {
	errFake := errors.New("fake error")
	fs := newMockOsFs(errFake, errFake, false, "123", os.TempDir())
	u := dataprovider.User{}
	u.Username = "test_username"
	u.Permissions = make(map[string][]string)
	u.Permissions["/"] = []string{dataprovider.PermAny}
	u.HomeDir = os.TempDir()
	c := Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, u, fs),
	}
	testfile := filepath.Join(u.HomeDir, "testfile")
	request := sftp.NewRequest("Remove", testfile)
	err := ioutil.WriteFile(testfile, []byte("test"), os.ModePerm)
	assert.NoError(t, err)
	_, err = c.Filewrite(request)
	assert.EqualError(t, err, sftp.ErrSSHFxFailure.Error())

	var flags sftp.FileOpenFlags
	flags.Write = true
	flags.Trunc = false
	flags.Append = true
	_, err = c.handleSFTPUploadToExistingFile(flags, testfile, testfile, 0, "/testfile", nil)
	assert.EqualError(t, err, sftp.ErrSSHFxOpUnsupported.Error())

	fs = newMockOsFs(errFake, nil, false, "123", os.TempDir())
	c.BaseConnection.Fs = fs
	err = c.handleSFTPRemove(testfile, request)
	assert.EqualError(t, err, sftp.ErrSSHFxFailure.Error())

	request = sftp.NewRequest("Rename", filepath.Base(testfile))
	request.Target = filepath.Base(testfile) + "1"
	err = c.Rename(testfile, testfile+"1", request.Filepath, request.Target)
	assert.EqualError(t, err, sftp.ErrSSHFxFailure.Error())

	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestUploadFiles(t *testing.T) {
	oldUploadMode := common.Config.UploadMode
	common.Config.UploadMode = common.UploadModeAtomic
	fs := vfs.NewOsFs("123", os.TempDir(), nil)
	u := dataprovider.User{}
	c := Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, u, fs),
	}
	var flags sftp.FileOpenFlags
	flags.Write = true
	flags.Trunc = true
	_, err := c.handleSFTPUploadToExistingFile(flags, "missing_path", "other_missing_path", 0, "/missing_path", nil)
	assert.Error(t, err, "upload to existing file must fail if one or both paths are invalid")

	common.Config.UploadMode = common.UploadModeStandard
	_, err = c.handleSFTPUploadToExistingFile(flags, "missing_path", "other_missing_path", 0, "/missing_path", nil)
	assert.Error(t, err, "upload to existing file must fail if one or both paths are invalid")

	missingFile := "missing/relative/file.txt"
	if runtime.GOOS == osWindows {
		missingFile = "missing\\relative\\file.txt"
	}
	_, err = c.handleSFTPUploadToNewFile(".", missingFile, "/missing", nil)
	assert.Error(t, err, "upload new file in missing path must fail")

	c.BaseConnection.Fs = newMockOsFs(nil, nil, false, "123", os.TempDir())
	f, err := ioutil.TempFile("", "temp")
	assert.NoError(t, err)
	err = f.Close()
	assert.NoError(t, err)

	tr, err := c.handleSFTPUploadToExistingFile(flags, f.Name(), f.Name(), 123, f.Name(), nil)
	if assert.NoError(t, err) {
		transfer := tr.(*transfer)
		transfers := c.GetTransfers()
		if assert.Equal(t, 1, len(transfers)) {
			assert.Equal(t, transfers[0].ID, transfer.GetID())
			assert.Equal(t, int64(123), transfer.InitialSize)
			err = transfer.Close()
			assert.NoError(t, err)
			assert.Equal(t, 0, len(c.GetTransfers()))
		}
	}
	err = os.Remove(f.Name())
	assert.NoError(t, err)
	common.Config.UploadMode = oldUploadMode
}

func TestWithInvalidHome(t *testing.T) {
	u := dataprovider.User{}
	u.HomeDir = "home_rel_path" //nolint:goconst
	_, err := loginUser(&u, dataprovider.LoginMethodPassword, "", nil)
	assert.Error(t, err, "login a user with an invalid home_dir must fail")

	u.HomeDir = os.TempDir()
	fs, err := u.GetFilesystem("123")
	assert.NoError(t, err)
	c := Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, u, fs),
	}
	_, err = c.Fs.ResolvePath("../upper_path")
	assert.Error(t, err, "tested path is not a home subdir")
	_, err = c.StatVFS(&sftp.Request{
		Method:   "StatVFS",
		Filepath: "../unresolvable-path",
	})
	assert.Error(t, err)
}

func TestSFTPCmdTargetPath(t *testing.T) {
	u := dataprovider.User{}
	if runtime.GOOS == osWindows {
		u.HomeDir = "C:\\invalid_home"
	} else {
		u.HomeDir = "/invalid_home"
	}
	u.Username = "testuser"
	u.Permissions = make(map[string][]string)
	u.Permissions["/"] = []string{dataprovider.PermAny}
	fs, err := u.GetFilesystem("123")
	assert.NoError(t, err)
	connection := Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, u, fs),
	}
	_, err = connection.getSFTPCmdTargetPath("invalid_path")
	assert.True(t, os.IsNotExist(err))
}

func TestSFTPGetUsedQuota(t *testing.T) {
	u := dataprovider.User{}
	u.HomeDir = "home_rel_path"
	u.Username = "test_invalid_user"
	u.QuotaSize = 4096
	u.QuotaFiles = 1
	u.Permissions = make(map[string][]string)
	u.Permissions["/"] = []string{dataprovider.PermAny}
	connection := Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, u, nil),
	}
	quotaResult := connection.HasSpace(false, false, "/")
	assert.False(t, quotaResult.HasSpace)
}

func TestSupportedSSHCommands(t *testing.T) {
	cmds := GetSupportedSSHCommands()
	assert.Equal(t, len(supportedSSHCommands), len(cmds))

	for _, c := range cmds {
		assert.True(t, utils.IsStringInSlice(c, supportedSSHCommands))
	}
}

func TestSSHCommandPath(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
	}
	connection := &Connection{
		channel: &mockSSHChannel,
	}
	sshCommand := sshCommand{
		command:    "test",
		connection: connection,
		args:       []string{},
	}
	assert.Equal(t, "", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "/tmp/../path"}
	assert.Equal(t, "/path", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "/tmp/"}
	assert.Equal(t, "/tmp/", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "tmp/"}
	assert.Equal(t, "/tmp/", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "/tmp/../../../path"}
	assert.Equal(t, "/path", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", ".."}
	assert.Equal(t, "/", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "."}
	assert.Equal(t, "/", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "//"}
	assert.Equal(t, "/", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "../.."}
	assert.Equal(t, "/", sshCommand.getDestPath())

	sshCommand.args = []string{"-t", "/.."}
	assert.Equal(t, "/", sshCommand.getDestPath())

	sshCommand.args = []string{"-f", "/a space.txt"}
	assert.Equal(t, "/a space.txt", sshCommand.getDestPath())
}

func TestSSHParseCommandPayload(t *testing.T) {
	cmd := "command -a  -f  /ab\\ à/some\\ spaces\\ \\ \\(\\).txt"
	name, args, _ := parseCommandPayload(cmd)
	assert.Equal(t, "command", name)
	assert.Equal(t, 3, len(args))
	assert.Equal(t, "/ab à/some spaces  ().txt", args[2])

	_, _, err := parseCommandPayload("")
	assert.Error(t, err, "parsing invalid command must fail")
}

func TestSSHCommandErrors(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
	}
	server, client := net.Pipe()
	defer func() {
		err := server.Close()
		assert.NoError(t, err)
	}()
	defer func() {
		err := client.Close()
		assert.NoError(t, err)
	}()
	user := dataprovider.User{}
	user.Permissions = make(map[string][]string)
	user.Permissions["/"] = []string{dataprovider.PermAny}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	connection := Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
		channel:        &mockSSHChannel,
	}
	cmd := sshCommand{
		command:    "md5sum",
		connection: &connection,
		args:       []string{},
	}
	err = cmd.handle()
	assert.Error(t, err, "ssh command must fail, we are sending a fake error")

	cmd = sshCommand{
		command:    "md5sum",
		connection: &connection,
		args:       []string{"/../../test_file_ftp.dat"},
	}
	err = cmd.handle()
	assert.Error(t, err, "ssh command must fail, we are requesting an invalid path")

	cmd = sshCommand{
		command:    "git-receive-pack",
		connection: &connection,
		args:       []string{"/../../testrepo"},
	}
	err = cmd.handle()
	assert.Error(t, err, "ssh command must fail, we are requesting an invalid path")

	cmd.connection.User.HomeDir = filepath.Clean(os.TempDir())
	cmd.connection.User.QuotaFiles = 1
	cmd.connection.User.UsedQuotaFiles = 2
	fs, err = cmd.connection.User.GetFilesystem("123")
	assert.NoError(t, err)
	cmd.connection.Fs = fs
	err = cmd.handle()
	assert.EqualError(t, err, common.ErrQuotaExceeded.Error())

	cmd.connection.User.QuotaFiles = 0
	cmd.connection.User.UsedQuotaFiles = 0
	cmd.connection.User.Permissions = make(map[string][]string)
	cmd.connection.User.Permissions["/"] = []string{dataprovider.PermListItems}
	err = cmd.handle()
	assert.EqualError(t, err, common.ErrPermissionDenied.Error())

	cmd.connection.User.Permissions["/"] = []string{dataprovider.PermAny}
	cmd.command = "invalid_command"
	command, err := cmd.getSystemCommand()
	assert.NoError(t, err)

	err = cmd.executeSystemCommand(command)
	assert.Error(t, err, "invalid command must fail")

	command, err = cmd.getSystemCommand()
	assert.NoError(t, err)

	_, err = command.cmd.StderrPipe()
	assert.NoError(t, err)

	err = cmd.executeSystemCommand(command)
	assert.Error(t, err, "command must fail, pipe was already assigned")

	err = cmd.executeSystemCommand(command)
	assert.Error(t, err, "command must fail, pipe was already assigned")

	command, err = cmd.getSystemCommand()
	assert.NoError(t, err)

	_, err = command.cmd.StdoutPipe()
	assert.NoError(t, err)
	err = cmd.executeSystemCommand(command)
	assert.Error(t, err, "command must fail, pipe was already assigned")

	fs, err = user.GetFilesystem("123")
	assert.NoError(t, err)
	connection.Fs = fs

	cmd = sshCommand{
		command:    "sftpgo-remove",
		connection: &connection,
		args:       []string{"/../../src"},
	}
	err = cmd.handle()
	assert.Error(t, err, "ssh command must fail, we are requesting an invalid path")

	cmd = sshCommand{
		command:    "sftpgo-copy",
		connection: &connection,
		args:       []string{"/../../test_src", "."},
	}
	err = cmd.handle()
	assert.Error(t, err, "ssh command must fail, we are requesting an invalid path")

	cmd.connection.User.HomeDir = filepath.Clean(os.TempDir())
	fs, err = cmd.connection.User.GetFilesystem("123")
	assert.NoError(t, err)
	cmd.connection.Fs = fs
	_, _, err = cmd.resolveCopyPaths(".", "../adir")
	assert.Error(t, err)

	cmd = sshCommand{
		command:    "sftpgo-copy",
		connection: &connection,
		args:       []string{"src", "dst"},
	}
	cmd.connection.User.Permissions = make(map[string][]string)
	cmd.connection.User.Permissions["/"] = []string{dataprovider.PermDownload}
	src, dst, err := cmd.getCopyPaths()
	assert.NoError(t, err)
	assert.False(t, cmd.hasCopyPermissions(src, dst, nil))

	cmd.connection.User.Permissions = make(map[string][]string)
	cmd.connection.User.Permissions["/"] = []string{dataprovider.PermAny}
	if runtime.GOOS != osWindows {
		aDir := filepath.Join(os.TempDir(), "adir")
		err = os.MkdirAll(aDir, os.ModePerm)
		assert.NoError(t, err)
		tmpFile := filepath.Join(aDir, "testcopy")
		err = ioutil.WriteFile(tmpFile, []byte("aaa"), os.ModePerm)
		assert.NoError(t, err)
		err = os.Chmod(aDir, 0001)
		assert.NoError(t, err)
		err = cmd.checkCopyDestination(tmpFile)
		assert.Error(t, err)
		err = os.Chmod(aDir, os.ModePerm)
		assert.NoError(t, err)
		err = os.Remove(tmpFile)
		assert.NoError(t, err)
	}
}

func TestCommandsWithExtensionsFilter(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	user := dataprovider.User{
		Username: "test",
		HomeDir:  os.TempDir(),
		Status:   1,
	}
	user.Filters.FileExtensions = []dataprovider.ExtensionsFilter{
		{
			Path:              "/subdir",
			AllowedExtensions: []string{".jpg"},
			DeniedExtensions:  []string{},
		},
	}

	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
		channel:        &mockSSHChannel,
	}
	cmd := sshCommand{
		command:    "md5sum",
		connection: connection,
		args:       []string{"subdir/test.png"},
	}
	err = cmd.handleHashCommands()
	assert.EqualError(t, err, common.ErrPermissionDenied.Error())

	cmd = sshCommand{
		command:    "rsync",
		connection: connection,
		args:       []string{"--server", "-vlogDtprze.iLsfxC", ".", "/"},
	}
	_, err = cmd.getSystemCommand()
	assert.EqualError(t, err, errUnsupportedConfig.Error())

	cmd = sshCommand{
		command:    "git-receive-pack",
		connection: connection,
		args:       []string{"/subdir"},
	}
	_, err = cmd.getSystemCommand()
	assert.EqualError(t, err, errUnsupportedConfig.Error())

	cmd = sshCommand{
		command:    "git-receive-pack",
		connection: connection,
		args:       []string{"/subdir/dir"},
	}
	_, err = cmd.getSystemCommand()
	assert.EqualError(t, err, errUnsupportedConfig.Error())

	cmd = sshCommand{
		command:    "git-receive-pack",
		connection: connection,
		args:       []string{"/adir/subdir"},
	}
	_, err = cmd.getSystemCommand()
	assert.NoError(t, err)
}

func TestSSHCommandsRemoteFs(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
	}
	server, client := net.Pipe()
	defer func() {
		err := server.Close()
		assert.NoError(t, err)
	}()
	defer func() {
		err := client.Close()
		assert.NoError(t, err)
	}()
	user := dataprovider.User{}
	user.FsConfig = dataprovider.Filesystem{
		Provider: dataprovider.S3FilesystemProvider,
		S3Config: vfs.S3FsConfig{
			Bucket:   "s3bucket",
			Endpoint: "endpoint",
			Region:   "eu-west-1",
		},
	}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
		channel:        &mockSSHChannel,
	}
	cmd := sshCommand{
		command:    "md5sum",
		connection: connection,
		args:       []string{},
	}

	command, err := cmd.getSystemCommand()
	assert.NoError(t, err)

	err = cmd.executeSystemCommand(command)
	assert.Error(t, err, "command must fail for a non local filesystem")
	cmd = sshCommand{
		command:    "sftpgo-copy",
		connection: connection,
		args:       []string{},
	}
	err = cmd.handeSFTPGoCopy()
	assert.Error(t, err)
	cmd = sshCommand{
		command:    "sftpgo-remove",
		connection: connection,
		args:       []string{},
	}
	err = cmd.handeSFTPGoRemove()
	assert.Error(t, err)
}

func TestGitVirtualFolders(t *testing.T) {
	permissions := make(map[string][]string)
	permissions["/"] = []string{dataprovider.PermAny}
	user := dataprovider.User{
		Permissions: permissions,
		HomeDir:     os.TempDir(),
	}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	conn := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
	}
	cmd := sshCommand{
		command:    "git-receive-pack",
		connection: conn,
		args:       []string{"/vdir"},
	}
	cmd.connection.User.VirtualFolders = append(cmd.connection.User.VirtualFolders, vfs.VirtualFolder{
		BaseVirtualFolder: vfs.BaseVirtualFolder{
			MappedPath: os.TempDir(),
		},
		VirtualPath: "/vdir",
	})
	_, err = cmd.getSystemCommand()
	assert.NoError(t, err)
	cmd.args = []string{"/"}
	_, err = cmd.getSystemCommand()
	assert.EqualError(t, err, errUnsupportedConfig.Error())
	cmd.args = []string{"/vdir1"}
	_, err = cmd.getSystemCommand()
	assert.NoError(t, err)

	cmd.connection.User.VirtualFolders = nil
	cmd.connection.User.VirtualFolders = append(cmd.connection.User.VirtualFolders, vfs.VirtualFolder{
		BaseVirtualFolder: vfs.BaseVirtualFolder{
			MappedPath: os.TempDir(),
		},
		VirtualPath: "/vdir",
	})
	cmd.args = []string{"/vdir/subdir"}
	_, err = cmd.getSystemCommand()
	assert.NoError(t, err)

	cmd.args = []string{"/adir/subdir"}
	_, err = cmd.getSystemCommand()
	assert.NoError(t, err)
}

func TestRsyncOptions(t *testing.T) {
	permissions := make(map[string][]string)
	permissions["/"] = []string{dataprovider.PermAny}
	user := dataprovider.User{
		Permissions: permissions,
		HomeDir:     os.TempDir(),
	}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	conn := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
	}
	sshCmd := sshCommand{
		command:    "rsync",
		connection: conn,
		args:       []string{"--server", "-vlogDtprze.iLsfxC", ".", "/"},
	}
	cmd, err := sshCmd.getSystemCommand()
	assert.NoError(t, err)
	assert.True(t, utils.IsStringInSlice("--safe-links", cmd.cmd.Args),
		"--safe-links must be added if the user has the create symlinks permission")

	permissions["/"] = []string{dataprovider.PermDownload, dataprovider.PermUpload, dataprovider.PermCreateDirs,
		dataprovider.PermListItems, dataprovider.PermOverwrite, dataprovider.PermDelete, dataprovider.PermRename}
	user.Permissions = permissions
	fs, err = user.GetFilesystem("123")
	assert.NoError(t, err)

	conn = &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
	}
	sshCmd = sshCommand{
		command:    "rsync",
		connection: conn,
		args:       []string{"--server", "-vlogDtprze.iLsfxC", ".", "/"},
	}
	cmd, err = sshCmd.getSystemCommand()
	assert.NoError(t, err)
	assert.True(t, utils.IsStringInSlice("--munge-links", cmd.cmd.Args),
		"--munge-links must be added if the user has the create symlinks permission")

	sshCmd.connection.User.VirtualFolders = append(sshCmd.connection.User.VirtualFolders, vfs.VirtualFolder{
		BaseVirtualFolder: vfs.BaseVirtualFolder{
			MappedPath: os.TempDir(),
		},
		VirtualPath: "/vdir",
	})
	_, err = sshCmd.getSystemCommand()
	assert.EqualError(t, err, errUnsupportedConfig.Error())
}

func TestSystemCommandSizeForPath(t *testing.T) {
	permissions := make(map[string][]string)
	permissions["/"] = []string{dataprovider.PermAny}
	user := dataprovider.User{
		Permissions: permissions,
		HomeDir:     os.TempDir(),
	}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	conn := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
	}
	sshCmd := sshCommand{
		command:    "rsync",
		connection: conn,
		args:       []string{"--server", "-vlogDtprze.iLsfxC", ".", "/"},
	}
	_, _, err = sshCmd.getSizeForPath("missing path")
	assert.NoError(t, err)
	testDir := filepath.Join(os.TempDir(), "dir")
	err = os.MkdirAll(testDir, os.ModePerm)
	assert.NoError(t, err)
	testFile := filepath.Join(testDir, "testfile")
	err = ioutil.WriteFile(testFile, []byte("test content"), os.ModePerm)
	assert.NoError(t, err)
	err = os.Symlink(testFile, testFile+".link")
	assert.NoError(t, err)
	numFiles, size, err := sshCmd.getSizeForPath(testFile + ".link")
	assert.NoError(t, err)
	assert.Equal(t, 0, numFiles)
	assert.Equal(t, int64(0), size)
	numFiles, size, err = sshCmd.getSizeForPath(testFile)
	assert.NoError(t, err)
	assert.Equal(t, 1, numFiles)
	assert.Equal(t, int64(12), size)
	if runtime.GOOS != osWindows {
		err = os.Chmod(testDir, 0001)
		assert.NoError(t, err)
		_, _, err = sshCmd.getSizeForPath(testFile)
		assert.Error(t, err)
		err = os.Chmod(testDir, os.ModePerm)
		assert.NoError(t, err)
	}
	err = os.RemoveAll(testDir)
	assert.NoError(t, err)
}

func TestSystemCommandErrors(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   writeErr,
	}
	server, client := net.Pipe()
	defer func() {
		err := server.Close()
		assert.NoError(t, err)
	}()
	defer func() {
		err := client.Close()
		assert.NoError(t, err)
	}()
	permissions := make(map[string][]string)
	permissions["/"] = []string{dataprovider.PermAny}
	homeDir := filepath.Join(os.TempDir(), "adir")
	err := os.MkdirAll(homeDir, os.ModePerm)
	assert.NoError(t, err)
	err = ioutil.WriteFile(filepath.Join(homeDir, "afile"), []byte("content"), os.ModePerm)
	assert.NoError(t, err)
	user := dataprovider.User{
		Permissions: permissions,
		HomeDir:     homeDir,
	}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
		channel:        &mockSSHChannel,
	}
	var sshCmd sshCommand
	if runtime.GOOS == osWindows {
		sshCmd = sshCommand{
			command:    "dir",
			connection: connection,
			args:       []string{"/"},
		}
	} else {
		sshCmd = sshCommand{
			command:    "ls",
			connection: connection,
			args:       []string{"/"},
		}
	}
	systemCmd, err := sshCmd.getSystemCommand()
	assert.NoError(t, err)

	systemCmd.cmd.Dir = os.TempDir()
	// FIXME: the command completes but the fake client is unable to read the response
	// no error is reported in this case. We can see that the expected code is executed
	// reading the test coverage
	sshCmd.executeSystemCommand(systemCmd) //nolint:errcheck

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   nil,
	}
	sshCmd.connection.channel = &mockSSHChannel
	baseTransfer := common.NewBaseTransfer(nil, sshCmd.connection.BaseConnection, nil, "", "", common.TransferDownload,
		0, 0, 0, false, fs)
	transfer := newTransfer(baseTransfer, nil, nil, nil)
	destBuff := make([]byte, 65535)
	dst := bytes.NewBuffer(destBuff)
	_, err = transfer.copyFromReaderToWriter(dst, sshCmd.connection.channel)
	assert.EqualError(t, err, readErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}
	sshCmd.connection.channel = &mockSSHChannel
	transfer.MaxWriteSize = 1
	_, err = transfer.copyFromReaderToWriter(dst, sshCmd.connection.channel)
	assert.EqualError(t, err, common.ErrQuotaExceeded.Error())

	mockSSHChannel = MockChannel{
		Buffer:        bytes.NewBuffer(buf),
		StdErrBuffer:  bytes.NewBuffer(stdErrBuf),
		ReadError:     nil,
		WriteError:    nil,
		ShortWriteErr: true,
	}
	sshCmd.connection.channel = &mockSSHChannel
	_, err = transfer.copyFromReaderToWriter(sshCmd.connection.channel, dst)
	assert.EqualError(t, err, io.ErrShortWrite.Error())
	transfer.MaxWriteSize = -1
	_, err = transfer.copyFromReaderToWriter(sshCmd.connection.channel, dst)
	assert.EqualError(t, err, common.ErrQuotaExceeded.Error())
	err = os.RemoveAll(homeDir)
	assert.NoError(t, err)
}

func TestGetConnectionInfo(t *testing.T) {
	c := common.ConnectionStatus{
		Username:      "test_user",
		ConnectionID:  "123",
		ClientVersion: "client",
		RemoteAddress: "127.0.0.1:1234",
		Protocol:      common.ProtocolSSH,
		Command:       "sha1sum /test_file_ftp.dat",
	}
	info := c.GetConnectionInfo()
	assert.Contains(t, info, "sha1sum /test_file_ftp.dat")
}

func TestSCPFileMode(t *testing.T) {
	mode := getFileModeAsString(0, true)
	assert.Equal(t, "0755", mode)

	mode = getFileModeAsString(0700, true)
	assert.Equal(t, "0700", mode)

	mode = getFileModeAsString(0750, true)
	assert.Equal(t, "0750", mode)

	mode = getFileModeAsString(0777, true)
	assert.Equal(t, "0777", mode)

	mode = getFileModeAsString(0640, false)
	assert.Equal(t, "0640", mode)

	mode = getFileModeAsString(0600, false)
	assert.Equal(t, "0600", mode)

	mode = getFileModeAsString(0, false)
	assert.Equal(t, "0644", mode)

	fileMode := uint32(0777)
	fileMode = fileMode | uint32(os.ModeSetgid)
	fileMode = fileMode | uint32(os.ModeSetuid)
	fileMode = fileMode | uint32(os.ModeSticky)
	mode = getFileModeAsString(os.FileMode(fileMode), false)
	assert.Equal(t, "7777", mode)

	fileMode = uint32(0644)
	fileMode = fileMode | uint32(os.ModeSetgid)
	mode = getFileModeAsString(os.FileMode(fileMode), false)
	assert.Equal(t, "4644", mode)

	fileMode = uint32(0600)
	fileMode = fileMode | uint32(os.ModeSetuid)
	mode = getFileModeAsString(os.FileMode(fileMode), false)
	assert.Equal(t, "2600", mode)

	fileMode = uint32(0044)
	fileMode = fileMode | uint32(os.ModeSticky)
	mode = getFileModeAsString(os.FileMode(fileMode), false)
	assert.Equal(t, "1044", mode)
}

func TestSCPUploadError(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   writeErr,
	}
	user := dataprovider.User{
		HomeDir:     filepath.Join(os.TempDir()),
		Permissions: make(map[string][]string),
	}
	user.Permissions["/"] = []string{dataprovider.PermAny}
	fs := vfs.NewOsFs("", user.HomeDir, nil)

	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-t", "/"},
		},
	}
	err := scpCommand.handle()
	assert.EqualError(t, err, writeErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer([]byte("D0755 0 testdir\n")),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   writeErr,
	}
	err = scpCommand.handleRecursiveUpload()
	assert.EqualError(t, err, writeErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer([]byte("D0755 a testdir\n")),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}
	err = scpCommand.handleRecursiveUpload()
	assert.Error(t, err)
}

func TestSCPInvalidEndDir(t *testing.T) {
	stdErrBuf := make([]byte, 65535)
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer([]byte("E\n")),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, dataprovider.User{}, fs),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-t", "/tmp"},
		},
	}
	err := scpCommand.handleRecursiveUpload()
	assert.EqualError(t, err, "unacceptable end dir command")
}

func TestSCPParseUploadMessage(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, dataprovider.User{}, fs),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-t", "/tmp"},
		},
	}
	_, _, err := scpCommand.parseUploadMessage("invalid")
	assert.Error(t, err, "parsing invalid upload message must fail")

	_, _, err = scpCommand.parseUploadMessage("D0755 0")
	assert.Error(t, err, "parsing incomplete upload message must fail")

	_, _, err = scpCommand.parseUploadMessage("D0755 invalidsize testdir")
	assert.Error(t, err, "parsing upload message with invalid size must fail")

	_, _, err = scpCommand.parseUploadMessage("D0755 0 ")
	assert.Error(t, err, "parsing upload message with invalid name must fail")
}

func TestSCPProtocolMessages(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, dataprovider.User{}, nil),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-t", "/tmp"},
		},
	}
	_, err := scpCommand.readProtocolMessage()
	assert.EqualError(t, err, readErr.Error())

	err = scpCommand.sendConfirmationMessage()
	assert.EqualError(t, err, writeErr.Error())

	err = scpCommand.sendProtocolMessage("E\n")
	assert.EqualError(t, err, writeErr.Error())

	_, err = scpCommand.getNextUploadProtocolMessage()
	assert.EqualError(t, err, readErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer([]byte("T1183832947 0 1183833773 0\n")),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   writeErr,
	}
	scpCommand.connection.channel = &mockSSHChannel
	_, err = scpCommand.getNextUploadProtocolMessage()
	assert.EqualError(t, err, writeErr.Error())

	respBuffer := []byte{0x02}
	protocolErrorMsg := "protocol error msg"
	respBuffer = append(respBuffer, protocolErrorMsg...)
	respBuffer = append(respBuffer, 0x0A)
	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(respBuffer),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}
	scpCommand.connection.channel = &mockSSHChannel
	err = scpCommand.readConfirmationMessage()
	if assert.Error(t, err) {
		assert.Equal(t, protocolErrorMsg, err.Error())
	}
}

func TestSCPTestDownloadProtocolMessages(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, dataprovider.User{}, nil),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-f", "-p", "/tmp"},
		},
	}
	path := "testDir"
	err := os.Mkdir(path, os.ModePerm)
	assert.NoError(t, err)
	stat, err := os.Stat(path)
	assert.NoError(t, err)
	err = scpCommand.sendDownloadProtocolMessages(path, stat)
	assert.EqualError(t, err, writeErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   nil,
	}

	err = scpCommand.sendDownloadProtocolMessages(path, stat)
	assert.EqualError(t, err, readErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	scpCommand.args = []string{"-f", "/tmp"}
	scpCommand.connection.channel = &mockSSHChannel
	err = scpCommand.sendDownloadProtocolMessages(path, stat)
	assert.EqualError(t, err, writeErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   nil,
	}
	scpCommand.connection.channel = &mockSSHChannel
	err = scpCommand.sendDownloadProtocolMessages(path, stat)
	assert.EqualError(t, err, readErr.Error())

	err = os.Remove(path)
	assert.NoError(t, err)
}

func TestSCPCommandHandleErrors(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	server, client := net.Pipe()
	defer func() {
		err := server.Close()
		assert.NoError(t, err)
	}()
	defer func() {
		err := client.Close()
		assert.NoError(t, err)
	}()
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, dataprovider.User{}, nil),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-f", "/tmp"},
		},
	}
	err := scpCommand.handle()
	assert.EqualError(t, err, readErr.Error())
	scpCommand.args = []string{"-i", "/tmp"}
	err = scpCommand.handle()
	assert.Error(t, err, "invalid scp command must fail")
}

func TestSCPErrorsMockFs(t *testing.T) {
	errFake := errors.New("fake error")
	fs := newMockOsFs(errFake, errFake, false, "1234", os.TempDir())
	u := dataprovider.User{}
	u.Username = "test"
	u.Permissions = make(map[string][]string)
	u.Permissions["/"] = []string{dataprovider.PermAny}
	u.HomeDir = os.TempDir()
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
	}
	server, client := net.Pipe()
	defer func() {
		err := server.Close()
		assert.NoError(t, err)
	}()
	defer func() {
		err := client.Close()
		assert.NoError(t, err)
	}()
	connection := &Connection{
		channel:        &mockSSHChannel,
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, u, fs),
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-r", "-t", "/tmp"},
		},
	}
	err := scpCommand.handleUpload("test", 0)
	assert.EqualError(t, err, errFake.Error())

	testfile := filepath.Join(u.HomeDir, "testfile")
	err = ioutil.WriteFile(testfile, []byte("test"), os.ModePerm)
	assert.NoError(t, err)
	stat, err := os.Stat(u.HomeDir)
	assert.NoError(t, err)
	err = scpCommand.handleRecursiveDownload(u.HomeDir, stat)
	assert.EqualError(t, err, errFake.Error())

	scpCommand.sshCommand.connection.Fs = newMockOsFs(errFake, nil, true, "123", os.TempDir())
	err = scpCommand.handleUpload(filepath.Base(testfile), 0)
	assert.EqualError(t, err, errFake.Error())

	err = scpCommand.handleUploadFile(testfile, testfile, 0, false, 4, "/testfile")
	assert.NoError(t, err)
	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestSCPRecursiveDownloadErrors(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	server, client := net.Pipe()
	defer func() {
		err := server.Close()
		assert.NoError(t, err)
	}()
	defer func() {
		err := client.Close()
		assert.NoError(t, err)
	}()
	fs := vfs.NewOsFs("123", os.TempDir(), nil)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, dataprovider.User{}, fs),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-r", "-f", "/tmp"},
		},
	}
	path := "testDir"
	err := os.Mkdir(path, os.ModePerm)
	assert.NoError(t, err)
	stat, err := os.Stat(path)
	assert.NoError(t, err)
	err = scpCommand.handleRecursiveDownload("invalid_dir", stat)
	assert.EqualError(t, err, writeErr.Error())

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}
	scpCommand.connection.channel = &mockSSHChannel
	err = scpCommand.handleRecursiveDownload("invalid_dir", stat)
	assert.Error(t, err, "recursive upload download must fail for a non existing dir")

	err = os.Remove(path)
	assert.NoError(t, err)
}

func TestSCPRecursiveUploadErrors(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, dataprovider.User{}, nil),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-r", "-t", "/tmp"},
		},
	}
	err := scpCommand.handleRecursiveUpload()
	assert.Error(t, err, "recursive upload must fail, we send a fake error message")

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   nil,
	}
	scpCommand.connection.channel = &mockSSHChannel
	err = scpCommand.handleRecursiveUpload()
	assert.Error(t, err, "recursive upload must fail, we send a fake error message")
}

func TestSCPCreateDirs(t *testing.T) {
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	u := dataprovider.User{}
	u.HomeDir = "home_rel_path"
	u.Username = "test"
	u.Permissions = make(map[string][]string)
	u.Permissions["/"] = []string{dataprovider.PermAny}
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}
	fs, err := u.GetFilesystem("123")
	assert.NoError(t, err)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, u, fs),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-r", "-t", "/tmp"},
		},
	}
	err = scpCommand.handleCreateDir("invalid_dir")
	assert.Error(t, err, "create invalid dir must fail")
}

func TestSCPDownloadFileData(t *testing.T) {
	testfile := "testfile"
	buf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	stdErrBuf := make([]byte, 65535)
	mockSSHChannelReadErr := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   nil,
	}
	mockSSHChannelWriteErr := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   writeErr,
	}
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, dataprovider.User{},
			vfs.NewOsFs("", os.TempDir(), nil)),
		channel: &mockSSHChannelReadErr,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-r", "-f", "/tmp"},
		},
	}
	err := ioutil.WriteFile(testfile, []byte("test"), os.ModePerm)
	assert.NoError(t, err)
	stat, err := os.Stat(testfile)
	assert.NoError(t, err)
	err = scpCommand.sendDownloadFileData(testfile, stat, nil)
	assert.EqualError(t, err, readErr.Error())

	scpCommand.connection.channel = &mockSSHChannelWriteErr
	err = scpCommand.sendDownloadFileData(testfile, stat, nil)
	assert.EqualError(t, err, writeErr.Error())

	scpCommand.args = []string{"-r", "-p", "-f", "/tmp"}
	err = scpCommand.sendDownloadFileData(testfile, stat, nil)
	assert.EqualError(t, err, writeErr.Error())

	scpCommand.connection.channel = &mockSSHChannelReadErr
	err = scpCommand.sendDownloadFileData(testfile, stat, nil)
	assert.EqualError(t, err, readErr.Error())

	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestSCPUploadFiledata(t *testing.T) {
	testfile := "testfile"
	buf := make([]byte, 65535)
	stdErrBuf := make([]byte, 65535)
	readErr := fmt.Errorf("test read error")
	writeErr := fmt.Errorf("test write error")
	mockSSHChannel := MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   writeErr,
	}
	user := dataprovider.User{
		Username: "testuser",
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, user, fs),
		channel:        &mockSSHChannel,
	}
	scpCommand := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
			args:       []string{"-r", "-t", "/tmp"},
		},
	}
	file, err := os.Create(testfile)
	assert.NoError(t, err)

	baseTransfer := common.NewBaseTransfer(file, scpCommand.connection.BaseConnection, nil, file.Name(),
		"/"+testfile, common.TransferDownload, 0, 0, 0, true, fs)
	transfer := newTransfer(baseTransfer, nil, nil, nil)

	err = scpCommand.getUploadFileData(2, transfer)
	assert.Error(t, err, "upload must fail, we send a fake write error message")

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    readErr,
		WriteError:   nil,
	}
	scpCommand.connection.channel = &mockSSHChannel
	file, err = os.Create(testfile)
	assert.NoError(t, err)
	transfer.File = file
	transfer.isFinished = false
	transfer.Connection.AddTransfer(transfer)
	err = scpCommand.getUploadFileData(2, transfer)
	assert.Error(t, err, "upload must fail, we send a fake read error message")

	respBuffer := []byte("12")
	respBuffer = append(respBuffer, 0x02)
	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(respBuffer),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}
	scpCommand.connection.channel = &mockSSHChannel
	file, err = os.Create(testfile)
	assert.NoError(t, err)
	baseTransfer.File = file
	transfer = newTransfer(baseTransfer, nil, nil, nil)
	transfer.Connection.AddTransfer(transfer)
	err = scpCommand.getUploadFileData(2, transfer)
	assert.Error(t, err, "upload must fail, we have not enough data to read")

	// the file is already closed so we have an error on trasfer closing
	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}

	transfer.Connection.AddTransfer(transfer)
	err = scpCommand.getUploadFileData(0, transfer)
	if assert.Error(t, err) {
		assert.EqualError(t, err, common.ErrTransferClosed.Error())
	}

	mockSSHChannel = MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
		ReadError:    nil,
		WriteError:   nil,
	}

	transfer.Connection.AddTransfer(transfer)
	err = scpCommand.getUploadFileData(2, transfer)
	assert.True(t, errors.Is(err, os.ErrClosed))

	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestUploadError(t *testing.T) {
	oldUploadMode := common.Config.UploadMode
	common.Config.UploadMode = common.UploadModeAtomic

	user := dataprovider.User{
		Username: "testuser",
	}
	fs := vfs.NewOsFs("", os.TempDir(), nil)
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSCP, user, fs),
	}

	testfile := "testfile"
	fileTempName := "temptestfile"
	file, err := os.Create(fileTempName)
	assert.NoError(t, err)
	baseTransfer := common.NewBaseTransfer(file, connection.BaseConnection, nil, testfile,
		testfile, common.TransferUpload, 0, 0, 0, true, fs)
	transfer := newTransfer(baseTransfer, nil, nil, nil)

	errFake := errors.New("fake error")
	transfer.TransferError(errFake)
	err = transfer.Close()
	if assert.Error(t, err) {
		assert.EqualError(t, err, common.ErrGenericFailure.Error())
	}
	if assert.Error(t, transfer.ErrTransfer) {
		assert.EqualError(t, transfer.ErrTransfer, errFake.Error())
	}
	assert.Equal(t, int64(0), transfer.BytesReceived)

	assert.NoFileExists(t, testfile)
	assert.NoFileExists(t, fileTempName)

	common.Config.UploadMode = oldUploadMode
}

func TestTransferFailingReader(t *testing.T) {
	user := dataprovider.User{
		Username: "testuser",
	}
	user.Permissions = make(map[string][]string)
	user.Permissions["/"] = []string{dataprovider.PermAny}

	fs := newMockOsFs(nil, nil, true, "", os.TempDir())
	connection := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
	}

	request := sftp.NewRequest("Open", "afile.txt")
	request.Flags = 27 // read,write,create,truncate

	transfer, err := connection.handleFilewrite(request)
	require.NoError(t, err)
	buf := make([]byte, 32)
	_, err = transfer.ReadAt(buf, 0)
	assert.EqualError(t, err, sftp.ErrSSHFxOpUnsupported.Error())
	if c, ok := transfer.(io.Closer); ok {
		err = c.Close()
		assert.NoError(t, err)
	}

	fsPath := filepath.Join(os.TempDir(), "afile.txt")

	r, _, err := pipeat.Pipe()
	assert.NoError(t, err)
	baseTransfer := common.NewBaseTransfer(nil, connection.BaseConnection, nil, fsPath, filepath.Base(fsPath), common.TransferUpload, 0, 0, 0, false, fs)
	errRead := errors.New("read is not allowed")
	tr := newTransfer(baseTransfer, nil, r, errRead)
	_, err = tr.ReadAt(buf, 0)
	assert.EqualError(t, err, errRead.Error())

	err = tr.Close()
	assert.NoError(t, err)

	tr = newTransfer(baseTransfer, nil, nil, errRead)
	_, err = tr.ReadAt(buf, 0)
	assert.EqualError(t, err, errRead.Error())

	err = tr.Close()
	assert.NoError(t, err)

	err = os.Remove(fsPath)
	assert.NoError(t, err)
	assert.Len(t, connection.GetTransfers(), 0)
}

func TestConnectionStatusStruct(t *testing.T) {
	var transfers []common.ConnectionTransfer
	transferUL := common.ConnectionTransfer{
		OperationType: "upload",
		StartTime:     utils.GetTimeAsMsSinceEpoch(time.Now()),
		Size:          123,
		VirtualPath:   "/test.upload",
	}
	transferDL := common.ConnectionTransfer{
		OperationType: "download",
		StartTime:     utils.GetTimeAsMsSinceEpoch(time.Now()),
		Size:          123,
		VirtualPath:   "/test.download",
	}
	transfers = append(transfers, transferUL)
	transfers = append(transfers, transferDL)
	c := common.ConnectionStatus{
		Username:       "test",
		ConnectionID:   "123",
		ClientVersion:  "fakeClient-1.0.0",
		RemoteAddress:  "127.0.0.1:1234",
		ConnectionTime: utils.GetTimeAsMsSinceEpoch(time.Now()),
		LastActivity:   utils.GetTimeAsMsSinceEpoch(time.Now()),
		Protocol:       "SFTP",
		Transfers:      transfers,
	}
	durationString := c.GetConnectionDuration()
	assert.NotEqual(t, 0, len(durationString))

	transfersString := c.GetTransfersAsString()
	assert.NotEqual(t, 0, len(transfersString))

	connInfo := c.GetConnectionInfo()
	assert.NotEqual(t, 0, len(connInfo))
}

func TestLoadHostKeys(t *testing.T) {
	configDir := ".."
	serverConfig := &ssh.ServerConfig{}
	c := Configuration{}
	c.HostKeys = []string{".", "missing file"}
	err := c.checkAndLoadHostKeys(configDir, serverConfig)
	assert.Error(t, err)
	testfile := filepath.Join(os.TempDir(), "invalidkey")
	err = ioutil.WriteFile(testfile, []byte("some bytes"), os.ModePerm)
	assert.NoError(t, err)
	c.HostKeys = []string{testfile}
	err = c.checkAndLoadHostKeys(configDir, serverConfig)
	assert.Error(t, err)
	err = os.Remove(testfile)
	assert.NoError(t, err)
	keysDir := filepath.Join(os.TempDir(), "keys")
	err = os.MkdirAll(keysDir, os.ModePerm)
	assert.NoError(t, err)
	rsaKeyName := filepath.Join(keysDir, defaultPrivateRSAKeyName)
	ecdsaKeyName := filepath.Join(keysDir, defaultPrivateECDSAKeyName)
	ed25519KeyName := filepath.Join(keysDir, defaultPrivateEd25519KeyName)
	nonDefaultKeyName := filepath.Join(keysDir, "akey")
	c.HostKeys = []string{nonDefaultKeyName, rsaKeyName, ecdsaKeyName, ed25519KeyName}
	err = c.checkAndLoadHostKeys(configDir, serverConfig)
	assert.Error(t, err)
	assert.FileExists(t, rsaKeyName)
	assert.FileExists(t, ecdsaKeyName)
	assert.FileExists(t, ed25519KeyName)
	assert.NoFileExists(t, nonDefaultKeyName)
	err = os.Remove(rsaKeyName)
	assert.NoError(t, err)
	err = os.Remove(ecdsaKeyName)
	assert.NoError(t, err)
	err = os.Remove(ed25519KeyName)
	assert.NoError(t, err)
	if runtime.GOOS != osWindows {
		err = os.Chmod(keysDir, 0551)
		assert.NoError(t, err)
		c.HostKeys = nil
		err = c.checkAndLoadHostKeys(keysDir, serverConfig)
		assert.Error(t, err)
		c.HostKeys = []string{rsaKeyName, ecdsaKeyName}
		err = c.checkAndLoadHostKeys(configDir, serverConfig)
		assert.Error(t, err)
		c.HostKeys = []string{ecdsaKeyName, rsaKeyName}
		err = c.checkAndLoadHostKeys(configDir, serverConfig)
		assert.Error(t, err)
		c.HostKeys = []string{ed25519KeyName}
		err = c.checkAndLoadHostKeys(configDir, serverConfig)
		assert.Error(t, err)
		err = os.Chmod(keysDir, 0755)
		assert.NoError(t, err)
	}
	err = os.RemoveAll(keysDir)
	assert.NoError(t, err)
}

func TestCertCheckerInitErrors(t *testing.T) {
	c := Configuration{}
	c.TrustedUserCAKeys = []string{".", "missing file"}
	err := c.initializeCertChecker("")
	assert.Error(t, err)
	testfile := filepath.Join(os.TempDir(), "invalidkey")
	err = ioutil.WriteFile(testfile, []byte("some bytes"), os.ModePerm)
	assert.NoError(t, err)
	c.TrustedUserCAKeys = []string{testfile}
	err = c.initializeCertChecker("")
	assert.Error(t, err)
	err = os.Remove(testfile)
	assert.NoError(t, err)
}

func TestRecursiveCopyErrors(t *testing.T) {
	permissions := make(map[string][]string)
	permissions["/"] = []string{dataprovider.PermAny}
	user := dataprovider.User{
		Permissions: permissions,
		HomeDir:     os.TempDir(),
	}
	fs, err := user.GetFilesystem("123")
	assert.NoError(t, err)
	conn := &Connection{
		BaseConnection: common.NewBaseConnection("", common.ProtocolSFTP, user, fs),
	}
	sshCmd := sshCommand{
		command:    "sftpgo-copy",
		connection: conn,
		args:       []string{"adir", "another"},
	}
	// try to copy a missing directory
	err = sshCmd.checkRecursiveCopyPermissions("adir", "another", "/another")
	assert.Error(t, err)
}

func TestSFTPSubSystem(t *testing.T) {
	permissions := make(map[string][]string)
	permissions["/"] = []string{dataprovider.PermAny}
	user := &dataprovider.User{
		Permissions: permissions,
		HomeDir:     os.TempDir(),
	}
	user.FsConfig.Provider = dataprovider.AzureBlobFilesystemProvider
	err := ServeSubSystemConnection(user, "connID", nil, nil)
	assert.Error(t, err)
	user.FsConfig.Provider = dataprovider.LocalFilesystemProvider

	buf := make([]byte, 0, 4096)
	stdErrBuf := make([]byte, 0, 4096)
	mockSSHChannel := &MockChannel{
		Buffer:       bytes.NewBuffer(buf),
		StdErrBuffer: bytes.NewBuffer(stdErrBuf),
	}
	// this is 327680 and it will result in packet too long error
	_, err = mockSSHChannel.Write([]byte{0x00, 0x05, 0x00, 0x00, 0x00, 0x00})
	assert.NoError(t, err)
	err = ServeSubSystemConnection(user, "id", mockSSHChannel, mockSSHChannel)
	assert.EqualError(t, err, "packet too long")

	subsystemChannel := newSubsystemChannel(mockSSHChannel, mockSSHChannel)
	n, err := subsystemChannel.Write([]byte{0x00})
	assert.NoError(t, err)
	assert.Equal(t, n, 1)
	err = subsystemChannel.Close()
	assert.NoError(t, err)
}

func TestRecoverer(t *testing.T) {
	c := Configuration{}
	c.AcceptInboundConnection(nil, nil)
	connID := "connectionID"
	connection := &Connection{
		BaseConnection: common.NewBaseConnection(connID, common.ProtocolSFTP, dataprovider.User{}, nil),
	}
	c.handleSftpConnection(nil, connection)
	sshCmd := sshCommand{
		command:    "cd",
		connection: connection,
	}
	err := sshCmd.handle()
	assert.EqualError(t, err, common.ErrGenericFailure.Error())
	scpCmd := scpCommand{
		sshCommand: sshCommand{
			command:    "scp",
			connection: connection,
		},
	}
	err = scpCmd.handle()
	assert.EqualError(t, err, common.ErrGenericFailure.Error())
	assert.Len(t, common.Connections.GetStats(), 0)
}

func TestListernerAcceptErrors(t *testing.T) {
	errFake := errors.New("a fake error")
	listener := newFakeListener(errFake)
	c := Configuration{}
	err := c.serve(listener, nil)
	require.EqualError(t, err, errFake.Error())
	err = listener.Close()
	require.NoError(t, err)

	errNetFake := &fakeNetError{error: errFake}
	listener = newFakeListener(errNetFake)
	err = c.serve(listener, nil)
	require.EqualError(t, err, errFake.Error())
	err = listener.Close()
	require.NoError(t, err)
}

type fakeNetError struct {
	error
	count int
}

func (e *fakeNetError) Timeout() bool {
	return false
}

func (e *fakeNetError) Temporary() bool {
	e.count++
	return e.count < 10
}

func (e *fakeNetError) Error() string {
	return e.error.Error()
}

type fakeListener struct {
	server net.Conn
	client net.Conn
	err    error
}

func (l *fakeListener) Accept() (net.Conn, error) {
	return l.client, l.err
}

func (l *fakeListener) Close() error {
	errClient := l.client.Close()
	errServer := l.server.Close()
	if errServer != nil {
		return errServer
	}
	return errClient
}

func (l *fakeListener) Addr() net.Addr {
	return l.server.LocalAddr()
}

func newFakeListener(err error) net.Listener {
	server, client := net.Pipe()

	return &fakeListener{
		server: server,
		client: client,
		err:    err,
	}
}
