import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import { Boxes, LayoutGrid, LogOut, Moon, SlidersHorizontal, Sun } from 'lucide-react';
import { useApp } from '@/store';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import type { MainTab } from '@/types';

function MainTabs({ className, layoutId }: { className?: string; layoutId: string }) {
  const tab = useApp((state) => state.tab);
  const setTab = useApp((state) => state.setTab);
  const reduce = useReducedMotion();

  const items: Array<{ value: MainTab; label: string; icon: typeof LayoutGrid }> = [
    { value: 'directory', label: '工作空间', icon: LayoutGrid },
    { value: 'studio', label: '编排设计器', icon: SlidersHorizontal },
  ];

  return (
    <nav
      aria-label="主导航"
      className={cn(
        'inline-flex h-9 items-center gap-0.5 rounded-lg border border-border bg-muted/50 p-1',
        className,
      )}
    >
      {items.map((item) => {
        const active = tab === item.value;
        const Icon = item.icon;
        return (
          <button
            key={item.value}
            type="button"
            aria-current={active ? 'page' : undefined}
            onClick={() => setTab(item.value)}
            className={cn(
              'relative inline-flex h-7 flex-1 items-center justify-center gap-2 whitespace-nowrap rounded-md px-3 text-[13px] font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/40 sm:flex-none',
              active ? 'text-foreground' : 'text-muted-foreground hover:text-foreground',
            )}
          >
            {active && (
              <motion.span
                layoutId={layoutId}
                className="absolute inset-0 rounded-md border border-border/60 bg-card shadow-xs"
                transition={reduce ? { duration: 0 } : { type: 'spring', stiffness: 480, damping: 38 }}
              />
            )}
            <span className="relative z-10 inline-flex items-center gap-2">
              <Icon className="size-3.5" strokeWidth={1.9} />
              {item.label}
            </span>
          </button>
        );
      })}
    </nav>
  );
}

export function Header() {
  const theme = useApp((state) => state.theme);
  const toggleTheme = useApp((state) => state.toggleTheme);
  const setTab = useApp((state) => state.setTab);
  const setDirectoryFilter = useApp((state) => state.setDirectoryFilter);
  const running = useApp(
    (state) => state.directory.items.filter((item) => (item.phase || '').toLowerCase() === 'running').length,
  );
  const auth = useApp((state) => state.auth);
  const askConfirm = useApp((state) => state.askConfirm);
  const logout = useApp((state) => state.logout);
  const reduce = useReducedMotion();

  const showLogout = auth.authEnabled && auth.authenticated;

  return (
    <header className="sticky top-0 z-40 w-full border-b border-border bg-background/85 backdrop-blur-md">
      <div className="mx-auto flex h-14 w-full max-w-[1600px] items-center gap-2 px-3 sm:gap-4 sm:px-5">
        <button
          type="button"
          onClick={() => setTab('directory')}
          className="flex min-w-0 shrink-0 items-center gap-2.5 rounded-md text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
          aria-label="返回工作空间列表"
        >
          <span className="grid size-8 shrink-0 place-items-center rounded-lg border border-border bg-secondary text-foreground shadow-xs">
            <Boxes className="size-4" strokeWidth={1.9} />
          </span>
          <span className="flex min-w-0 flex-col">
            <span className="truncate text-[13px] font-semibold leading-tight tracking-tight">
              BocomWork
            </span>
            <span className="hidden truncate text-[10px] leading-tight text-muted-foreground sm:block">
              工作空间控制台
            </span>
          </span>
        </button>

        <div className="hidden flex-1 justify-center md:flex">
          <MainTabs layoutId="main-tab-desktop" />
        </div>

        <div className="flex flex-1 items-center justify-end gap-1 md:flex-none">
          <button
            type="button"
            onClick={() => {
              setTab('directory');
              setDirectoryFilter('running');
            }}
            className="hidden h-8 items-center gap-2 rounded-full border border-success/25 bg-success/8 px-2.5 text-xs font-medium text-success outline-none transition-colors hover:bg-success/12 focus-visible:ring-2 focus-visible:ring-success/30 sm:inline-flex"
            title="查看运行中的工作空间"
          >
            <span className="size-1.5 rounded-full bg-success status-pulse" aria-hidden="true" />
            运行中
            <span className="tabular-nums font-semibold">{running}</span>
          </button>

          <Button
            variant="ghost"
            size="iconSm"
            onClick={toggleTheme}
            aria-label={theme === 'dark' ? '切换到浅色主题' : '切换到深色主题'}
            title={theme === 'dark' ? '切换到浅色主题' : '切换到深色主题'}
          >
            <AnimatePresence initial={false} mode="wait">
              <motion.span
                key={theme}
                initial={reduce ? false : { rotate: -60, opacity: 0, scale: 0.8 }}
                animate={{ rotate: 0, opacity: 1, scale: 1 }}
                exit={reduce ? { opacity: 0 } : { rotate: 60, opacity: 0, scale: 0.8 }}
                transition={{ duration: 0.16 }}
                className="inline-flex"
              >
                {theme === 'dark' ? <Moon className="size-4" /> : <Sun className="size-4" />}
              </motion.span>
            </AnimatePresence>
          </Button>

          {showLogout && (
            <Button
              variant="ghost"
              size="iconSm"
              aria-label="退出登录"
              title="退出登录"
              onClick={() =>
                askConfirm({
                  title: '退出登录',
                  description: '确定要退出当前控制台会话吗？',
                  confirmLabel: '退出登录',
                  onConfirm: () => logout(),
                })
              }
            >
              <LogOut className="size-4" />
            </Button>
          )}
        </div>
      </div>

      <div className="border-t border-border/70 px-3 py-1.5 md:hidden">
        <MainTabs className="w-full" layoutId="main-tab-mobile" />
      </div>
    </header>
  );
}
