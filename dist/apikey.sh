#!/bin/bash

USERNAME="${OA}"
API_KEY=$(curl -s --request GET \
	  --url "http://53.12.9.18/bocomcode-omservice/apikey/query?username=${USERNAME}" \
	    --header 'Accept: */*' \
	      --header 'Accept-Encoding: gzip, deflate, br' \
	        --header 'Connection: keep-alive' \
		  --header 'User-Agent: PostmanRuntime-ApipostRuntime/1.1.0' | grep -o '"api_key":"[^"]*"' | cut -d'"' -f4)

mkdir -p /root/.local/share/opencode
mkdir -p /root/.bocomcode
mkdir -p /root/.config/opencode
mkdir -p /root/.pi/agent

cat > /root/.local/share/opencode/auth.json << EOF
{
	  "bocomproxy": {
	      "type": "api",
    "key": "$API_KEY",
    "remoteLiteLLMInfoUrl": "http://53.12.9.18/bocomcode-omservice/api/models"
  },
  "bocomproxy-test": {
      "type": "api",
          "key": "$API_KEY"
	    }
}
EOF

cat > /root/.bocomcode/bocomcode.json << 'EOF'
{
  "$schema": "https://opencode.ai/config.json",
  "enabled_providers": [
    "bocomproxy"
  ],
  "provider": {
    "bocomproxy": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://53.12.9.18:80/litellm/v1"
      }
    }
  },
  "bocomConfig": {
      "debugMode": true
  }
}
EOF

cat > /root/.config/opencode.json << 'EOF'
{
  "$schema": "https://opencode.ai/config.json",
  "autoupdate": false,
  "plugin": [
    "file:///opt/opencode-plugins/node_modules/@devtheops/opencode-plugin-otel"
  ],
  "env": {
    "OA":"{env:OA}"
  },
  "experimental": {
    "openTelemetry": true
  }
}
EOF

cat > /root/.pi/agent/models.json << 'EOF'
{
  "providers": {
    "bocomproxy": {
      "baseUrl": "http://53.12.0.12:4000/v1",
      "api": "openai-completions",
      "models": [
        {
          "id": "MiniMax M2.5",
          "name": "MiniMax M2.5"
        }
      ]
    }
  }
}
EOF

cat > /root/.pi/agent/auth.json << EOF
{
    "bocomproxy":{
        "type": "api_key",
         "key": "$API_KEY"
    }
}
EOF

echo "Created ~/.local/share/opencode/auth.json"
echo "Created ~/config/bocomcode.json"
echo "Created ~/config/opencode.json"
