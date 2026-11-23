FROM bocomwork-musl:v5

# 非 root 运行（Workspace 注解 ai.example.com/run-as-user=10001）所需的属主设置：
# 这里不写 USER，是否以非 root 运行只由 operator 注入的 runAsUser 决定。

# Replace the bocomwork frontend PDF asset in the image.
# move.sh 热更新时会 rm -rf /app/out /app/packages 再解压，这些目录必须归 10001。
# 不对整个 /app 递归 chown（node_modules 很大，会让该层体积翻倍）。
COPY --chown=10001:10001 dist/ /app/
RUN chmod +x /app/scripts/start-prod.sh
COPY bocomwork.json  /root/.bocomwork/bocomwork.json
# move.sh 热更新会覆盖该文件，文件属主必须是 10001（/usr/local/bin 目录本身无需可写）
COPY --chown=10001:10001 opencode /usr/local/bin/
COPY apikey.sh /app/apikey.sh
COPY move.sh /app/move.sh
# apikey.sh 会往 /root/.pi/agent/ 写 models.json、auth.json，目录必须归 10001
COPY --chown=10001:10001 APPEND_SYSTEM.md /root/.pi/agent/APPEND_SYSTEM.md
RUN chown 10001:10001 /root/.pi/agent

CMD ["/bin/sh","-c","/app/apikey.sh && /app/move.sh && /app/scripts/start-prod.sh"]
