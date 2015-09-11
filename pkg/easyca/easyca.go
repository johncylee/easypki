package easyca

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var (
	// Index format
	// 0 full string
	// 1 Valid/Revoked/Expired
	// 2 Expiration date
	// 3 Revocation date
	// 4 Serial
	// 5 Filename
	// 6 Subject
	indexRegexp = regexp.MustCompile("^(V|R|E)\t([0-9]{12}Z)\t([0-9]{12}Z)?\t([0-9a-fA-F]{2,})\t([^\t]+)\t(.+)")
)

func GeneratePrivateKey(path string) (*rsa.PrivateKey, error) {
	keyFile, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create %v: %v", path, err)
	}
	defer keyFile.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate private key: %v", err)
	}
	err = pem.Encode(keyFile, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err != nil {
		return nil, fmt.Errorf("pem encode private key: %v", err)
	}
	return key, nil
}

func GenerateCertifcate(pkiroot, name string, template *x509.Certificate) error {
	// TODO(jclerc): check that pki has been init

	var crtPath string
	privateKeyPath := filepath.Join(pkiroot, "private", name+".key")
	if name == "ca" {
		crtPath = filepath.Join(pkiroot, name+".crt")
	} else {
		crtPath = filepath.Join(pkiroot, "issued", name+".crt")
	}

	var caCrt *x509.Certificate
	var caKey *rsa.PrivateKey

	if _, err := os.Stat(privateKeyPath); err == nil {
		return fmt.Errorf("a key pair for %v already exists", name)
	}

	privateKey, err := GeneratePrivateKey(privateKeyPath)
	if err != nil {
		return fmt.Errorf("generate private key: %v", err)
	}

	publicKeyBytes, err := asn1.Marshal(*privateKey.Public().(*rsa.PublicKey))
	if err != nil {
		return fmt.Errorf("marshal public key: %v", err)
	}
	subjectKeyId := sha1.Sum(publicKeyBytes)
	template.SubjectKeyId = subjectKeyId[:]

	template.NotBefore = time.Now()
	template.SignatureAlgorithm = x509.SHA256WithRSA
	if template.IsCA {
		serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
		serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
		if err != nil {
			return fmt.Errorf("failed to generate ca serial number: %s", err)
		}
		template.SerialNumber = serialNumber
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		template.BasicConstraintsValid = true
		template.Issuer = template.Subject
		template.AuthorityKeyId = template.SubjectKeyId

		caCrt = template
		caKey = privateKey
	} else {
		template.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
		serialNumber, err := NextSerial(pkiroot)
		if err != nil {
			return fmt.Errorf("get next serial: %v", err)
		}
		template.SerialNumber = serialNumber

		caCrt, caKey, err = GetCA(pkiroot)
		if err != nil {
			return fmt.Errorf("get ca: %v", err)
		}
	}

	crt, err := x509.CreateCertificate(rand.Reader, template, caCrt, privateKey.Public(), caKey)
	if err != nil {
		return fmt.Errorf("create certificate: %v", err)
	}

	crtFile, err := os.Create(crtPath)
	if err != nil {
		return fmt.Errorf("create %v: %v", crtPath, err)
	}
	defer crtFile.Close()

	err = pem.Encode(crtFile, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: crt,
	})
	if err != nil {
		return fmt.Errorf("pem encode crt: %v", err)
	}

	// I do not think we have to write the ca.crt in the index
	if !template.IsCA {
		WriteIndex(pkiroot, name, template)
		if err != nil {
			return fmt.Errorf("write index: %v", err)
		}
	}
	return nil
}

func GetCA(pkiroot string) (*x509.Certificate, *rsa.PrivateKey, error) {
	caKeyBytes, err := ioutil.ReadFile(filepath.Join(pkiroot, "private", "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("read ca private key: %v", err)
	}
	p, _ := pem.Decode(caKeyBytes)
	if p == nil {
		return nil, nil, fmt.Errorf("pem decode did not found pem encoded ca private key")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(p.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca private key: %v", err)
	}

	caCrtBytes, err := ioutil.ReadFile(filepath.Join(pkiroot, "ca.crt"))
	if err != nil {
		return nil, nil, fmt.Errorf("read ca crt: %v", err)
	}
	p, _ = pem.Decode(caCrtBytes)
	if p == nil {
		return nil, nil, fmt.Errorf("pem decode did not found pem encoded ca cert")
	}
	caCrt, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca crt: %v", err)
	}

	return caCrt, caKey, nil
}

func RevokeSerial(pkiroot string, serial *big.Int) error {
	f, err := os.OpenFile(filepath.Join(pkiroot, "index.txt"), os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		matches := indexRegexp.FindStringSubmatch(scanner.Text())
		if len(matches) != 7 {
			return fmt.Errorf("wrong line format")
		}
		matchedSerial := big.NewInt(0)
		fmt.Sscanf(matches[4], "%X", matchedSerial)
		if matchedSerial.Cmp(serial) == 0 {
			if matches[1] == "R" {
				return fmt.Errorf("certificate already revoked")
			}

			lines = append(lines, fmt.Sprintf("R\t%v\t%vZ\t%v\t%v\t%v",
				matches[2],
				time.Now().UTC().Format("060102150405"),
				matches[4],
				matches[5],
				matches[6]))
		} else {
			lines = append(lines, matches[0])
		}
	}

	f.Truncate(0)
	f.Seek(0, 0)

	for _, line := range lines {
		n, err := fmt.Fprintln(f, line)
		if err != nil {
			return fmt.Errorf("write line: %v", err)
		}
		if n == 0 {
			return fmt.Errorf("supposed to write [%v], written 0 bytes", line)
		}
	}
	return nil
}

func GetCertificate(path string) (*x509.Certificate, error) {
	crtBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read crt: %v", err)
	}
	p, _ := pem.Decode(crtBytes)
	if p == nil {
		return nil, fmt.Errorf("pem decode did not found pem encoded cert")
	}
	crt, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse crt: %v", err)
	}

	return crt, nil
}

func WriteIndex(pkiroot, filename string, crt *x509.Certificate) error {
	f, err := os.OpenFile(filepath.Join(pkiroot, "index.txt"), os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	serialOutput := fmt.Sprintf("%X", crt.SerialNumber)
	// For compatibility with openssl we need an even length
	if len(serialOutput)%2 == 1 {
		serialOutput = "0" + serialOutput
	}
	// subject: /C=FR/ST=IDF/O=Umbrella Corporation/CN=test.clerc.io
	// Date format: yymmddHHMMSSZ
	// E|R|V<tab>Expiry<tab>[RevocationDate]<tab>Serial<tab>filename<tab>SubjectDN
	n, err := fmt.Fprintf(f, "V\t%vZ\t\t%v\t%v.crt\t%v\n",
		crt.NotAfter.UTC().Format("060102150405"),
		serialOutput,
		filename,
		"/CN="+crt.Subject.CommonName)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("written 0 bytes in index file")
	}
	return nil
}

// |-ca.crt
// |-crlnumber
// |-index.txt
// |-index.txt.attr
// |-serial
// |-issued/
//   |- name.crt
// |-private
//   |- ca.key
//   |- name.key
func GeneratePKIStructure(pkiroot string) error {

	for _, dir := range []string{"private", "issued"} {
		err := os.Mkdir(filepath.Join(pkiroot, dir), 0755)
		if err != nil {
			return fmt.Errorf("creating dir %v: %v", dir, err)
		}
	}

	serial, err := os.Create(filepath.Join(pkiroot, "serial"))
	if err != nil {
		return fmt.Errorf("create serial: %v", err)
	}
	defer serial.Close()
	n, err := fmt.Fprintln(serial, "01")
	if err != nil {
		return fmt.Errorf("write serial: %v", err)
	}
	if n == 0 {
		return fmt.Errorf("write serial, written 0 bytes")
	}

	crlnumber, err := os.Create(filepath.Join(pkiroot, "crlnumber"))
	if err != nil {
		return fmt.Errorf("create crlnumber: %v", err)
	}
	defer crlnumber.Close()
	n, err = fmt.Fprintln(crlnumber, "01")
	if err != nil {
		return fmt.Errorf("write crlnumber: %v", err)
	}
	if n == 0 {
		return fmt.Errorf("write crlnumber, written 0 bytes")
	}

	index, err := os.Create(filepath.Join(pkiroot, "index.txt"))
	if err != nil {
		return fmt.Errorf("create index: %v", err)
	}
	defer index.Close()

	indexattr, err := os.Create(filepath.Join(pkiroot, "index.txt.attr"))
	if err != nil {
		return fmt.Errorf("create index.txt.attr: %v", err)
	}
	defer indexattr.Close()
	n, err = fmt.Fprintln(indexattr, "unique_subject = no")
	if err != nil {
		return fmt.Errorf("write index.txt.attr: %v", err)
	}
	if n == 0 {
		return fmt.Errorf("write index.txt.attr, written 0 bytes")
	}
	return nil
}