package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"strings"
)

// Gramine がエンクレーブ内に提供するアテステーション用擬似ファイル（SGX実行時のみ存在）
const (
	attestationTypePath = "/dev/attestation/attestation_type"
	userReportDataPath  = "/dev/attestation/user_report_data"
	myTargetInfoPath    = "/dev/attestation/my_target_info"
	targetInfoPath      = "/dev/attestation/target_info"
	quotePath           = "/dev/attestation/quote"
	reportPath          = "/dev/attestation/report"
)

// SGXのquote/reportからMRENCLAVE/MRSIGNERを取り出すためのオフセット。
// sgx_report_body_t 内では mr_enclave が先頭+64、mr_signer が先頭+128 に位置する。
// DCAP quote では 48 バイトのヘッダの後に report body が続く
const (
	reportBodySize        = 384
	quoteHeaderSize       = 48
	mrEnclaveOffsetInBody = 64
	mrSignerOffsetInBody  = 128
	measurementSize       = 32
)

// prover は削除証明の発行を担う（§6.4, §9）。
//
// 起動時に Ed25519 署名鍵ペアをエンクレーブ内で生成し、公開鍵の SHA-256 を
// user_report_data に埋め込んだ quote を一度だけ取得する。以後の削除証明は
// この鍵で署名する（毎回 quote を生成するコストを避ける運用、§9.3 手順2）。
// 署名鍵はメモリ上にのみ存在し、エンクレーブ外に出ることはない
type prover struct {
	priv            ed25519.PrivateKey
	pub             ed25519.PublicKey
	attestationType string // "none" / "dcap" / "epid"（/dev/attestation が無い環境では "none"）
	quoteB64        string // 起動時に取得した quote（取得不可なら空）
	mrenclave       string // hex
	mrsigner        string // hex
}

func newProver() *prover {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		// 乱数源が機能していない場合のみ。署名鍵無しでの運用は削除証明の意味を失うため停止する
		log.Fatalf("signing key generation failed: %v", err)
	}
	p := &prover{priv: priv, pub: pub, attestationType: "none"}

	if b, err := os.ReadFile(attestationTypePath); err == nil {
		p.attestationType = strings.TrimSpace(string(b))
	}

	// user_report_data（64バイト）の先頭32バイトに公開鍵の SHA-256 を埋め込む。
	// これにより quote が「真正なエンクレーブがこの署名鍵を保持している」ことの証明になる
	reportData := make([]byte, 64)
	pubHash := sha256.Sum256(p.pub)
	copy(reportData, pubHash[:])
	if err := os.WriteFile(userReportDataPath, reportData, 0o600); err != nil {
		// SGX外（開発環境・gramine-direct）での実行。削除証明は署名のみで発行される
		log.Printf("SGX attestation unavailable (%v): deletion certificates will not carry a quote", err)
		return p
	}

	// ローカルレポートの取得には自分自身を対象とした target_info の設定が必要
	if ti, err := os.ReadFile(myTargetInfoPath); err == nil {
		_ = os.WriteFile(targetInfoPath, ti, 0o600)
	}

	if q, err := os.ReadFile(quotePath); err == nil && len(q) >= quoteHeaderSize+reportBodySize {
		p.quoteB64 = base64.StdEncoding.EncodeToString(q)
		p.mrenclave = measurementAt(q, quoteHeaderSize+mrEnclaveOffsetInBody)
		p.mrsigner = measurementAt(q, quoteHeaderSize+mrSignerOffsetInBody)
		log.Printf("SGX quote acquired (%d bytes, mrenclave=%s)", len(q), p.mrenclave)
	} else if rep, err := os.ReadFile(reportPath); err == nil && len(rep) >= reportBodySize {
		// quote が取れない場合（sgx.remote_attestation = "none"）でもローカルレポートから
		// MRENCLAVE/MRSIGNER を取得して証明書に含める（quote は空になり第三者検証は不可）
		p.mrenclave = measurementAt(rep, mrEnclaveOffsetInBody)
		p.mrsigner = measurementAt(rep, mrSignerOffsetInBody)
		log.Printf("SGX local report acquired (mrenclave=%s); no quote (remote attestation disabled)", p.mrenclave)
	}
	return p
}

func measurementAt(b []byte, off int) string {
	return hex.EncodeToString(b[off : off+measurementSize])
}

// certificateCore は削除証明の署名対象部分。このフィールド順で JSON 化した
// バイト列が署名メッセージとなる（検証者が決定的に再構築できる形式）
type certificateCore struct {
	DataID      string `json:"data_id"`
	DeletedAt   string `json:"deleted_at"`
	ContentHash string `json:"content_hash"`
}

type enclaveReport struct {
	MRENCLAVE       string `json:"mrenclave"`
	MRSIGNER        string `json:"mrsigner"`
	Quote           string `json:"quote"`            // base64。user_report_data に sha256(public_key) が入っている
	AttestationType string `json:"attestation_type"` // "none" / "dcap" / "epid"
	PublicKey       string `json:"public_key"`       // base64（Ed25519 raw 32バイト）
	SignatureScheme string `json:"signature_scheme"` // "ed25519"
}

// deletionCertificate は削除証明（§9.2）。
// 検証手順（§9.3）: quote を検証 → quote 内の user_report_data と sha256(public_key) の
// 一致を確認 → signature を public_key で検証 → content_hash を登録時の値と照合
type deletionCertificate struct {
	DataID        string        `json:"data_id"`
	DeletedAt     string        `json:"deleted_at"`
	ContentHash   string        `json:"content_hash"`
	EnclaveReport enclaveReport `json:"enclave_report"`
	Signature     string        `json:"signature"` // base64。certificateCore の JSON への Ed25519 署名
}

// issueCertificate は削除イベントに対する削除証明を発行する
func (p *prover) issueCertificate(dataID, contentHash, deletedAt string) (json.RawMessage, error) {
	payload, err := json.Marshal(certificateCore{
		DataID:      dataID,
		DeletedAt:   deletedAt,
		ContentHash: contentHash,
	})
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(p.priv, payload)

	cert := deletionCertificate{
		DataID:      dataID,
		DeletedAt:   deletedAt,
		ContentHash: contentHash,
		EnclaveReport: enclaveReport{
			MRENCLAVE:       p.mrenclave,
			MRSIGNER:        p.mrsigner,
			Quote:           p.quoteB64,
			AttestationType: p.attestationType,
			PublicKey:       base64.StdEncoding.EncodeToString(p.pub),
			SignatureScheme: "ed25519",
		},
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
	return json.Marshal(cert)
}
