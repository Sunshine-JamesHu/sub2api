package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const rawCookie = `SEARCH_SAMESITE=CgQI8KEB; SID=g.a000EAm00QzbzzIAnUyNsnJQiVLCM9Cn8muWbCyhSLF-HdbPqNDThFEZHww6TuICYPDFQW5o1AACgYKAesSARcSFQHGX2MiM_aBB6sKYclPvcrMeicOyBoVAUF8yKqbw11Hs1nUcz8OVKciY9F10076; __Secure-1PSID=g.a000EAm00QzbzzIAnUyNsnJQiVLCM9Cn8muWbCyhSLF-HdbPqNDTTUlVn-5y4TRA9U7LTSqT4wACgYKAU0SARcSFQHGX2MiJB0Ogs69lSnZHR1sH0jH5BoVAUF8yKoX-N9UpqEYSkUEMbqkNzvN0076; __Secure-3PSID=g.a000EAm00QzbzzIAnUyNsnJQiVLCM9Cn8muWbCyhSLF-HdbPqNDTbU4vcbQKRvva0P2HMCen2QACgYKASoSARcSFQHGX2Miu7JvCXo5JuyzW1M8X6Z8OxoVAUF8yKr2TRUSfOk44W6jtXTrGaOj0076; HSID=AwIVkFbZJxHC7K58N; SSID=A1yVZOzi1ByXM-vFG; APISID=0P4uuepRDHKhVoSv/AFm6mKLDB7xZH0z0z; SAPISID=WJg84RWnoITZHmC1/AtUt3erOl9NeYp7CD; __Secure-1PAPISID=WJg84RWnoITZHmC1/AtUt3erOl9NeYp7CD; __Secure-3PAPISID=WJg84RWnoITZHmC1/AtUt3erOl9NeYp7CD; GCL_AW_P=GCL.1792771503.CjwKCAjwi-zWBhAdEiwAqlRf-OOvfq0g36nPm0I_QcWvGRVkyBpkTT4ruGj6RWTWla2jVN3egllNIBoCbFoQAvD_BwE; AEC=Aaa9EJp7U_ZlF-Sp7URyxK1-mDaTbcoV8TXdyM0D9zEXt18y4TLOvoSutOU; S=billing-ui-v3=aJkkjK3e6mH-XlKsBMf3aM80tkhBgb3i:billing-ui-v3-efe=aJkkjK3e6mH-XlKsBMf3aM80tkhBgb3i; NID=CvcDCAESqQMBM9Agl558U2fqyyUjoMxgy92bnZwDFpF1u_JKjA1mN1wrZTPG1ERmyUTZ4WVpj0lCXS-3ObDNCiV_VO2LLTMsdfDyopXES4BDybEOxLMYbB2_7Da8cYyZF7b3KYz8qp3ouo72Ka6jqmeKCYCQqtKe-mG0p-fy5SA62LjgCz0mODRTNzwfM38j-K4a-9Qs9N-fiLek5f4f86LYhFA7xHYAujrNTLN8W1tKpuLxgc3CdSwUge1nO197qz05Srm3fkvXqFlCr0YmSpKiYjRcIS3YYra2UMfrithB15RPYL6mWX1MNnDpq2sKE7u7wQZ9QxZaaFsZvClK-QKv9APeJZ9jGYJsGWSI0zgbBcZXAYzrPU5TfLlPigiOQ3RLE5x6xFm6-W1tBM9JXt0okNuHUmQoyPMlDgdFlaIWQ0B0u88bu7r9e9pkpFf2HlcWX4GGiQlQelb0bWkJduKfLC2Of8l8piWZEOSMiLVID3117I3Ir_yOl_3t4CGTAUhSSKzJFahXcB2u1xT59QT_ApdLLf_1Y4dUtVt-83Vnwgrxk9fbzwMmGy_t5GAuRSgBMkUBcEpCz3jEw7ngDQ8SylMY7I7vkYnEOKDdDSo7WcxQ3yxlPiBIOx02ZtOjsm2t1mcYGV6t5nB6D2OEROVXfozQfqdtkg8; __Secure-1PSIDTS=sidts-CjEBnJZzatJmLCbQnOGkfJsrZ3_BVPVVNdrYp4aprPcw2n6nBjcfcVu5XoLbxYyZYkstEAA; __Secure-3PSIDTS=sidts-CjEBnJZzatJmLCbQnOGkfJsrZ3_BVPVVNdrYp4aprPcw2n6nBjcfcVu5XoLbxYyZYkstEAA; SIDCC=APD_SKE1DcJFNnsmDCUBC9UpjsmesaVLXURVnE8S3beOSe9LlAV6fR-a5nstWS_XrVF9RyXNzA; __Secure-1PSIDCC=APD_SKGTGxgbNZAlQmsQ1GM22xqB_-rSOgTbrzySs4RDcToJUAScvO3d9qilxCtFjtwO8M65Vw; __Secure-3PSIDCC=APD_SKFlcHg98X7NPbxPkPMO3GHZq9wgxwErTzmidpLmIKXimY8vDfNPGldyjwWfIyFe0CiqRw`

func newVisitID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	uuid := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
	return "v1_" + base64.StdEncoding.EncodeToString([]byte(uuid))
}

func main() {
	proxyURL, _ := url.Parse("socks5://192.168.31.88:7895")
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 30 * time.Second,
	}

	ctx := context.Background()

	// 1. GetLoggingContext
	fmt.Println("1. Calling GetLoggingContext...")
	visitID := newVisitID()
	logReq, _ := http.NewRequestWithContext(ctx, "POST", aistudio.MakerSuiteRPCBase+"GetLoggingContext", strings.NewReader("[]"))
	headers, err := aistudio.BuildRequestHeaders(rawCookie, aistudio.DefaultUserAgent)
	if err != nil {
		panic(err)
	}
	headers.Set("X-AIStudio-Visit-Id", visitID)
	headers.Set("X-User-Agent", "grpc-web-javascript/0.1")
	logReq.Header = headers

	logResp, err := client.Do(logReq)
	if err != nil {
		panic(err)
	}
	defer logResp.Body.Close()
	logBody, _ := io.ReadAll(logResp.Body)
	fmt.Printf("GetLoggingContext Status: %d, Response: %s\n", logResp.StatusCode, string(logBody))

	// 2. Generate WAA proof
	manager := aistudio.DefaultWAAManager()
	prompt := "hi"
	digest := aistudio.ComputePayloadDigest(prompt)
	fmt.Printf("\n2. Generating WAA proof for prompt: %q...\n", prompt)

	proof, err := manager.GetProof(ctx, "test_account", rawCookie, "socks5://192.168.31.88:7895", digest, prompt)
	if err != nil {
		fmt.Printf("GetProof error: %v\n", err)
		return
	}
	fmt.Printf("WAA proof generated successfully! Prefix: %q, Length: %d\n", proof[:10], len(proof))

	// 3. Send GenerateContent with X-AIStudio-G1-Tier
	req := &antigravity.GeminiRequest{
		GenerationConfig: &antigravity.GeminiGenerationConfig{MaxOutputTokens: 8192},
		Contents: []antigravity.GeminiContent{
			{
				Role: "user",
				Parts: []antigravity.GeminiPart{
					{Text: prompt},
				},
			},
		},
	}
	model := "gemini-2.5-flash"
	jspbBody, err := aistudio.EncodeGenerateContentRequest(model, req, proof)
	if err != nil {
		panic(err)
	}

	targetURL := aistudio.MakerSuiteRPCBase + "GenerateContent"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(jspbBody))
	if err != nil {
		panic(err)
	}

	genHeaders := headers.Clone()
	genHeaders.Set("X-AIStudio-G1-Tier", "TIER1") // From BenefitTierPro
	httpReq.Header = genHeaders
	fmt.Println(string(jspbBody))
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Printf("Send request failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("GenerateContent HTTP Status: %d\n", resp.StatusCode)
	fmt.Printf("Response Body: %s\n", string(b))
}