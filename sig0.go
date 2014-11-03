// SIG(0)
//
// From RFC 2931:
//
//     SIG(0) provides protection for DNS transactions and requests ....
//     ... protection for glue records, DNS requests, protection for message headers
//     on requests and responses, and protection of the overall integrity of a response.
//
// It works like TSIG, except that SIG(0) uses public key cryptography, instead of the shared
// secret approach in TSIG.
package dns

import (
	"crypto"
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Sign signs a dns.Msg. It fills the signature with the appropriate data.
// The SIG record should have the SignerName, KeyTag, Algorithm, Inception
// and Expiration set.
func (rr *SIG) Sign(k PrivateKey, m *Msg) ([]byte, error) {
	if k == nil {
		return nil, ErrPrivKey
	}
	if rr.KeyTag == 0 || len(rr.SignerName) == 0 || rr.Algorithm == 0 {
		return nil, ErrKey
	}
	rr.Header().Rrtype = TypeSIG
	rr.Header().Class = ClassANY
	rr.Header().Ttl = 0
	rr.Header().Name = "."
	rr.OrigTtl = 0
	rr.TypeCovered = 0
	rr.Labels = 0

	buflen := m.Len() + rr.len()
	switch k := k.(type) {
	case *rsa.PrivateKey:
		buflen += len(k.N.Bytes())
	case *dsa.PrivateKey:
		buflen += 40
	case *ecdsa.PrivateKey:
		buflen += 96
	default:
		return nil, ErrPrivKey
	}
	buf := make([]byte, m.Len()+rr.len()+buflen)
	mbuf, err := m.PackBuffer(buf)
	if err != nil {
		return nil, err
	}
	if &buf[0] != &mbuf[0] {
		return nil, ErrBuf
	}
	off, err := PackRR(rr, buf, len(mbuf), nil, false)
	if err != nil {
		return nil, err
	}
	buf = buf[:off:cap(buf)]
	var hash crypto.Hash
	switch rr.Algorithm {
	case DSA, RSASHA1:
		hash = crypto.SHA1
	case RSASHA256, ECDSAP256SHA256:
		hash = crypto.SHA256
	case ECDSAP384SHA384:
		hash = crypto.SHA384
	case RSASHA512:
		hash = crypto.SHA512
	default:
		return nil, ErrAlg
	}
	hasher := hash.New()
	// Write SIG rdata
	hasher.Write(buf[len(mbuf)+1+2+2+4+2:])
	// Write message
	hasher.Write(buf[:len(mbuf)])
	hashed := hasher.Sum(nil)

	var sig []byte
	switch p := k.(type) {
	case *dsa.PrivateKey:
		t := byte((len(p.PublicKey.Y.Bytes()) - 64) / 8)
		r1, s1, err := dsa.Sign(rand.Reader, p, hashed)
		if err != nil {
			return nil, err
		}
		sig = make([]byte, 0, 1+len(r1.Bytes())+len(s1.Bytes()))
		sig = append(sig, t)
		sig = append(sig, r1.Bytes()...)
		sig = append(sig, s1.Bytes()...)
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, p, hash, hashed)
		if err != nil {
			return nil, err
		}
	case *ecdsa.PrivateKey:
		r1, s1, err := ecdsa.Sign(rand.Reader, p, hashed)
		if err != nil {
			return nil, err
		}
		sig = r1.Bytes()
		sig = append(sig, s1.Bytes()...)
	default:
		return nil, ErrAlg
	}
	rr.Signature = unpackBase64(sig)
	buf = append(buf, sig...)
	if len(buf) > int(^uint16(0)) {
		return nil, ErrBuf
	}
	// Adjust sig data length
	rdoff := len(mbuf) + 1 + 2 + 2 + 4
	rdlen, _ := unpackUint16(buf, rdoff)
	rdlen += uint16(len(sig))
	buf[rdoff], buf[rdoff+1] = packUint16(rdlen)
	// Adjust additional count
	adc, _ := unpackUint16(buf, 10)
	adc += 1
	buf[10], buf[11] = packUint16(adc)
	return buf, nil
}

// Verify validates the message buf using the key k.
// It's assumed that buf is a valid message from which rr was unpacked.
func (rr *SIG) Verify(k *KEY, buf []byte) error {
	if k == nil {
		return ErrKey
	}
	if rr.KeyTag == 0 || len(rr.SignerName) == 0 || rr.Algorithm == 0 {
		return ErrKey
	}

	var hash crypto.Hash
	switch rr.Algorithm {
	case DSA, RSASHA1:
		hash = crypto.SHA1
	case RSASHA256, ECDSAP256SHA256:
		hash = crypto.SHA256
	case ECDSAP384SHA384:
		hash = crypto.SHA384
	case RSASHA512:
		hash = crypto.SHA512
	default:
		return ErrAlg
	}
	hasher := hash.New()

	buflen := len(buf)
	qdc, _ := unpackUint16(buf, 4)
	anc, _ := unpackUint16(buf, 6)
	auc, _ := unpackUint16(buf, 8)
	adc, offset := unpackUint16(buf, 10)
	var err error
	for i := uint16(0); i < qdc && offset < buflen; i++ {
		// decode a name
		_, offset, err = UnpackDomainName(buf, offset)
		if err != nil {
			return err
		}
		// skip past Type and Class
		offset += 2 + 2
	}
	for i := uint16(1); i < anc+auc+adc && offset < buflen; i++ {
		// decode a name
		_, offset, err = UnpackDomainName(buf, offset)
		if err != nil {
			return err
		}
		// skip past Type, Class and TTL
		offset += 2 + 2 + 4
		var rdlen uint16
		rdlen, offset = unpackUint16(buf, offset)
		offset += int(rdlen)
	}
	// offset should be just prior to SIG
	bodyend := offset
	// Owner name SHOULD be root
	_, offset, err = UnpackDomainName(buf, offset)
	if err != nil {
		return err
	}
	// Skip Type, Class, TTL, RDLen
	offset += 2 + 2 + 4 + 2
	sigstart := offset
	offset += 2 + 1 + 1 + 4 // skip Type Covered, Algorithm, Labels, Original TTL
	// TODO: This should be moved out and used elsewhere
	unpackUint32 := func(buf []byte, off int) (uint32, int) {
		r := uint32(buf[off])<<24 | uint32(buf[off+1])<<16 | uint32(buf[off+2])<<8 | uint32(buf[off+3])
		return r, off + 4
	}
	var expire, incept uint32
	expire, offset = unpackUint32(buf, offset)
	incept, offset = unpackUint32(buf, offset)
	now := uint32(time.Now().Unix())
	if now < incept || now > expire {
		return ErrTime
	}
	offset += 2 // skip key tag
	var signername string
	signername, offset, err = UnpackDomainName(buf, offset)
	if err != nil {
		return err
	}
	// If key has come from the DNS name compression might
	// have mangled the case of the name
	if strings.ToLower(signername) != strings.ToLower(k.Header().Name) {
		return fmt.Errorf("Signer name doesn't match key name")
	}
	sigend := offset
	hasher.Write(buf[sigstart:sigend])
	hasher.Write(buf[:10])
	hasher.Write([]byte{
		byte((adc - 1) << 8),
		byte(adc - 1),
	})
	hasher.Write(buf[12:bodyend])

	hashed := hasher.Sum(nil)
	sig := buf[sigend:]
	switch k.Algorithm {
	case DSA:
		pk := k.publicKeyDSA()
		sig = sig[1:]
		r := big.NewInt(0)
		r.SetBytes(sig[:len(sig)/2])
		s := big.NewInt(0)
		s.SetBytes(sig[len(sig)/2:])
		if pk != nil {
			if dsa.Verify(pk, hashed, r, s) {
				return nil
			}
			return ErrSig
		}
	case RSASHA1, RSASHA256, RSASHA512:
		pk := k.publicKeyRSA()
		if pk != nil {
			return rsa.VerifyPKCS1v15(pk, hash, hashed, sig)
		}
	case ECDSAP256SHA256, ECDSAP384SHA384:
		pk := k.publicKeyCurve()
		r := big.NewInt(0)
		r.SetBytes(sig[:len(sig)/2])
		s := big.NewInt(0)
		s.SetBytes(sig[len(sig)/2:])
		if pk != nil {
			if ecdsa.Verify(pk, hashed, r, s) {
				return nil
			}
			return ErrSig
		}
	}
	return ErrKeyAlg
}