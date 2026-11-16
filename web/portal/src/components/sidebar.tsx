import { motion, useReducedMotion } from 'motion/react';
import { LayoutGrid, ShieldCheck, SlidersHorizontal } from 'lucide-react';
import { useApp } from '@/store';
import { cn } from '@/lib/utils';
import type { MainTab } from '@/types';

const NAV_ITEMS: Array<{ value: MainTab; label: string; icon: typeof LayoutGrid }> = [
  { value: 'directory', label: '工作空间', icon: LayoutGrid },
  { value: 'studio', label: '编排设计器', icon: SlidersHorizontal },
  { value: 'egress', label: '出站白名单', icon: ShieldCheck },
];

export function Sidebar() {
  const tab = useApp((state) => state.tab);
  const setTab = useApp((state) => state.setTab);
  const reduce = useReducedMotion();

  return (
    <aside className="flex w-60 shrink-0 flex-col border-r border-border bg-sidebar">
      <nav aria-label="主导航" className="flex flex-col gap-1.5 p-3">
        {NAV_ITEMS.map((item) => {
          const active = tab === item.value;
          const Icon = item.icon;
          return (
            <button
              key={item.value}
              type="button"
              aria-current={active ? 'page' : undefined}
              onClick={() => setTab(item.value)}
              className={cn(
                'relative flex h-11 items-center gap-3 rounded-lg px-3.5 text-sm outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/40',
                active
                  ? 'bg-info/8 font-semibold text-info'
                  : 'font-medium text-muted-foreground hover:bg-accent hover:text-foreground',
              )}
            >
              {active && (
                <motion.span
                  layoutId="sidebar-indicator"
                  className="absolute inset-y-2 left-0 w-[3px] rounded-full bg-info"
                  transition={reduce ? { duration: 0 } : { type: 'spring', stiffness: 480, damping: 38 }}
                />
              )}
              <Icon className="size-4 shrink-0" strokeWidth={1.9} />
              {item.label}
            </button>
          );
        })}
      </nav>
    </aside>
  );
}
