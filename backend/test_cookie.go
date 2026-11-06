package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
)

const rawCookie = `SEARCH_SAMESITE=CgQI8KEB; SID=g.a000EAm00QzbzzIAnUyNsnJQiVLCM9Cn8muWbCyhSLF-HdbPqNDThFEZHww6TuICYPDFQW5o1AACgYKAesSARcSFQHGX2MiM_aBB6sKYclPvcrMeicOyBoVAUF8yKqbw11Hs1nUcz8OVKciY9F10076; __Secure-1PSID=g.a000EAm00QzbzzIAnUyNsnJQiVLCM9Cn8muWbCyhSLF-HdbPqNDTTUlVn-5y4TRA9U7LTSqT4wACgYKAU0SARcSFQHGX2MiJB0Ogs69lSnZHR1sH0jH5BoVAUF8yKoX-N9UpqEYSkUEMbqkNzvN0076; __Secure-3PSID=g.a000EAm00QzbzzIAnUyNsnJQiVLCM9Cn8muWbCyhSLF-HdbPqNDTbU4vcbQKRvva0P2HMCen2QACgYKASoSARcSFQHGX2Miu7JvCXo5JuyzW1M8X6Z8OxoVAUF8yKr2TRUSfOk44W6jtXTrGaOj0076; HSID=AwIVkFbZJxHC7K58N; SSID=A1yVZOzi1ByXM-vFG; APISID=0P4uuepRDHKhVoSv/AFm6mKLDB7xZH0z0z; SAPISID=WJg84RWnoITZHmC1/AtUt3erOl9NeYp7CD; __Secure-1PAPISID=WJg84RWnoITZHmC1/AtUt3erOl9NeYp7CD; __Secure-3PAPISID=WJg84RWnoITZHmC1/AtUt3erOl9NeYp7CD; GCL_AW_P=GCL.1792771503.CjwKCAjwi-zWBhAdEiwAqlRf-OOvfq0g36nPm0I_QcWvGRVkyBpkTT4ruGj6RWTWla2jVN3egllNIBoCbFoQAvD_BwE; AEC=Aaa9EJp7U_ZlF-Sp7URyxK1-mDaTbcoV8TXdyM0D9zEXt18y4TLOvoSutOU; NID=CvcDCAESqQMBM9Agl5qgwue_sTVDTJYDOtSzecgHAkKdkTdKck9rwSPlcFkP5IwYuBtfu5Sy49FvElWSUS_lShCgzs8nPnwzEi8wdag7iEOJ-vyyp4eymqYG8dwgthNtK47TKAi4rXNfHMYA9_tLqQdsZqk_yVGKByXj8Yg6I8wanpyB5AXKbPTUQ0l9mcOsculnjmvV60w3fDq8WRTWfxqFIPYQvnWFMHEYVir8QzlhRO25BTeeR865pOyEBsUdj8UKR3MMX4_U5g2MKUyFjmRYp6Y4nD4UKoKO4ZdE1J60_-Nuvcxb5CDKC1YBGFPbpitSeEpowoblPfAzJGL6Lea1TNEtN66u0dBKu8ngg3kWdPGu-nCtR2wpFi3V3-M5KUf8d0_3rDNJcJim3rc-5FE-vENrnSphSqKgi_E4I9fon8GemQ8ssbIgGv4CNcfNOzDmx8pBwvHY0mfq0aEs7gN2_zVUWmEG-kDI8kN8jhbctqlZACRPIgeXD6nsovoQmaHd3PytVrNp2vbv4o_GrswwAbBlrryzoo9aboYrHa9OSXOMCW6SgsVCyu6PkQ3yqCgBMkUBcEpCz3jEw7ngDQ8SylMY7I7vkYnEOKDdDSo7WcxQ3yxlPiBIOx02ZtOjsm2t1mcYGV6t5nB6D2OEROVXfozQfqdtkg8; S=billing-ui-v3=aJkkjK3e6mH-XlKsBMf3aM80tkhBgb3i:billing-ui-v3-efe=aJkkjK3e6mH-XlKsBMf3aM80tkhBgb3i; __Secure-1PSIDTS=sidts-CjEBnJZzaqkTgZ6NxfNm38Ab3s_c2VRJ1zd9L-gcHg0PODU1YcqxPBgQZ2FR9cg2RsT3EAA; __Secure-3PSIDTS=sidts-CjEBnJZzaqkTgZ6NxfNm38Ab3s_c2VRJ1zd9L-gcHg0PODU1YcqxPBgQZ2FR9cg2RsT3EAA; SIDCC=APD_SKFnKczvsE5mDJzH3wkxcNn9XDEyYiyR7GsRxgPMzOz2HgoMldv4Tc48cBjB9yg0uDQqTw; __Secure-1PSIDCC=APD_SKFIhFXsMh0Q_WKqPddkHc0MyXcTK4kdohPIOPNLF1-RY6ZilikpqyRtLoHmQrhu3PnpOw; __Secure-3PSIDCC=APD_SKHjXPsHIp5n08LCeDdSX3yv3tezpzhdbMv-duwCXEijia-owanmpfkM63uqeo2brVzJFA`

var keyRegex = regexp.MustCompile(`"WIu0Nc":"([^"]+)"`)

func main() {
	proxyURL, _ := url.Parse("http://127.0.0.1:7890")
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 20 * time.Second,
	}

	ctx := context.Background()

	// 1. Fetch AI Studio page to extract client API Key (WIu0Nc)
	fmt.Println("--- 1. Fetching AI Studio page to get client API Key ---")
	pageReq, _ := http.NewRequestWithContext(ctx, "GET", "https://aistudio.google.com/prompts/new_chat", nil)
	pageReq.Header.Set("User-Agent", aistudio.DefaultUserAgent)
	pageReq.Header.Set("Cookie", rawCookie)

	pageResp, err := client.Do(pageReq)
	if err != nil {
		panic(err)
	}
	defer pageResp.Body.Close()
	pageBody, _ := io.ReadAll(pageResp.Body)
	fmt.Printf("Page Status: %d, Length: %d\n", pageResp.StatusCode, len(pageBody))

	apiKey := ""
	if match := keyRegex.FindSubmatch(pageBody); len(match) == 2 {
		apiKey = string(match[1])
		fmt.Printf("Found client API Key (WIu0Nc): %s\n", apiKey)
	} else {
		fmt.Println("Warning: WIu0Nc pattern not found in page body, falling back to default key")
		apiKey = "AIzaSyBGb5fGAyC-pRcRU6MUHb__b_vKha71HRE"
	}

	// 2. Test Benefit Tier with the client API Key
	fmt.Println("\n--- 2. Testing GetAiStudioBenefitTier with x-goog-api-key ---")
	tierReq, err := http.NewRequestWithContext(ctx, "POST", aistudio.MakerSuiteRPCBase+"GetAiStudioBenefitTier", bytes.NewReader([]byte("[]")))
	if err != nil {
		panic(err)
	}
	headers, err := aistudio.BuildRequestHeaders(rawCookie, "")
	if err != nil {
		panic(err)
	}
	tierReq.Header = headers
	tierReq.Header.Set("x-goog-api-key", apiKey)

	tierResp, err := client.Do(tierReq)
	if err != nil {
		fmt.Printf("GetAiStudioBenefitTier failed: %v\n", err)
	} else {
		defer tierResp.Body.Close()
		b, _ := io.ReadAll(tierResp.Body)
		fmt.Printf("Status: %d\nResponse: %s\n", tierResp.StatusCode, string(b))
	}

	// 3. Test ListModels with the client API Key
	fmt.Println("\n--- 3. Testing ListModels ---")
	modelsReq, _ := http.NewRequestWithContext(ctx, "POST", aistudio.MakerSuiteRPCBase+"ListModels", bytes.NewReader([]byte("[]")))
	modelsReq.Header = headers.Clone()
	modelsReq.Header.Set("x-goog-api-key", apiKey)

	modelsResp, err := client.Do(modelsReq)
	if err != nil {
		fmt.Printf("ListModels failed: %v\n", err)
	} else {
		defer modelsResp.Body.Close()
		b, _ := io.ReadAll(modelsResp.Body)
		fmt.Printf("ListModels Status: %d, Response Length: %d\n", modelsResp.StatusCode, len(b))
		if len(b) > 200 {
			fmt.Printf("Preview: %s...\n", string(b[:200]))
		} else {
			fmt.Printf("Response: %s\n", string(b))
		}
	}
}
