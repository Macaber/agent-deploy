import { useEffect, useState } from 'react';
import { Boxes, Eye, EyeOff, Loader2 } from 'lucide-react';
import { useApp } from '@/store';
import { Modal } from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';

export function LoginDialog() {
  const auth = useApp((state) => state.auth);
  const login = useApp((state) => state.login);
  const closeLogin = useApp((state) => state.closeLogin);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);

  useEffect(() => {
    if (!auth.loginOpen) {
      setPassword('');
      setShowPassword(false);
    }
  }, [auth.loginOpen]);

  return (
    <Modal
      open={auth.loginOpen}
      onOpenChange={(open) => {
        if (!open) closeLogin();
      }}
      title={
        <span className="flex items-center gap-2.5">
          <span className="grid size-8 place-items-center rounded-lg border border-border bg-secondary text-foreground">
            <Boxes className="size-4" strokeWidth={1.9} />
          </span>
          登录工作空间控制台
        </span>
      }
      description="BocomWork · 请输入管理员凭据以继续访问集群中的工作空间。"
      className="max-w-sm"
      hideClose
      preventClose
    >
      <form
        className="flex flex-col gap-3.5"
        onSubmit={(event) => {
          event.preventDefault();
          void login(username, password);
        }}
      >
        <Field label="用户名" htmlFor="login-username">
          <Input
            id="login-username"
            autoFocus
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            placeholder="请输入用户名"
            autoComplete="username"
            spellCheck={false}
          />
        </Field>

        <Field label="密码" htmlFor="login-password">
          <div className="relative">
            <Input
              id="login-password"
              type={showPassword ? 'text' : 'password'}
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              placeholder="请输入密码"
              autoComplete="current-password"
              className="pr-9"
            />
            <button
              type="button"
              onClick={() => setShowPassword((value) => !value)}
              aria-label={showPassword ? '隐藏密码' : '显示密码'}
              className="absolute right-1.5 top-1/2 grid size-6 -translate-y-1/2 place-items-center rounded text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40"
            >
              {showPassword ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
            </button>
          </div>
        </Field>

        {auth.loginError && (
          <p className="text-[11px] leading-snug text-destructive" role="alert">
            {auth.loginError}
          </p>
        )}

        <Button type="submit" size="lg" className="mt-1 w-full" disabled={auth.loginSubmitting}>
          {auth.loginSubmitting && <Loader2 className="size-4 animate-spin" />}
          {auth.loginSubmitting ? '正在登录…' : '立即登录'}
        </Button>
      </form>
    </Modal>
  );
}
